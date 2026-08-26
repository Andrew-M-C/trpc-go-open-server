package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	jsonvalue "github.com/Andrew-M-C/go.jsonvalue"
	"github.com/Andrew-M-C/trpc-go-utils/concurrent"
	"github.com/Andrew-M-C/trpc-go-utils/log"
)

// modelRoute 是 ModelConfig 的运行时形式：预解析好 host / extra / 反向代理。
type modelRoute struct {
	name          string
	upstreamModel string
	scheme        string            // http / https
	host          string            // host[:port]
	extra         *jsonvalue.V      // 保证是 object 或 nil
	keyMappings   map[string]string // Authorization Bearer token 映射表
	pathMappings  map[string]PathMappingConfigSetting
	proxy         *httputil.ReverseProxy
}

// proxyHandler 负责处理 /v1/ 下的所有请求。
type proxyHandler struct {
	routes          map[string]*modelRoute
	modelsList      []byte // 预序列化好的 GET /v1/models 响应体
	anthropicClient *http.Client
}

// newProxyHandler 根据配置构建 handler，会校验 name 唯一、URI / extra 合法。
func newProxyHandler(cfg *Config) (*proxyHandler, error) {
	if cfg == nil || len(cfg.Models) == 0 {
		return nil, fmt.Errorf("ai-proxy: no models configured")
	}

	transport := sharedTransport()

	routes := make(map[string]*modelRoute, len(cfg.Models))
	for i := range cfg.Models {
		m := cfg.Models[i]
		if m.Name == "" {
			return nil, fmt.Errorf("ai-proxy: models[%d].name is empty", i)
		}
		if m.Model == "" {
			return nil, fmt.Errorf("ai-proxy: models[%d](%s).model is empty", i, m.Name)
		}
		if m.Host == "" {
			return nil, fmt.Errorf("ai-proxy: models[%d](%s).host is empty", i, m.Name)
		}
		if _, dup := routes[m.Name]; dup {
			return nil, fmt.Errorf("ai-proxy: duplicated model name %q", m.Name)
		}

		u, err := url.Parse(m.Host)
		if err != nil {
			return nil, fmt.Errorf("ai-proxy: parse host for model %q: %w", m.Name, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("ai-proxy: invalid host %q for model %q (expect e.g. https://example.com)", m.Host, m.Name)
		}
		if u.Path != "" && u.Path != "/" {
			return nil, fmt.Errorf("ai-proxy: host %q for model %q must not contain path", m.Host, m.Name)
		}

		var extra *jsonvalue.V
		if m.Extra != "" {
			extra, err = jsonvalue.UnmarshalString(m.Extra)
			if err != nil {
				return nil, fmt.Errorf("ai-proxy: parse extra for model %q: %w", m.Name, err)
			}
			if !extra.IsObject() {
				return nil, fmt.Errorf("ai-proxy: extra for model %q must be a JSON object", m.Name)
			}
		}

		route := &modelRoute{
			name:          m.Name,
			upstreamModel: m.Model,
			scheme:        u.Scheme,
			host:          u.Host,
			extra:         extra,
			keyMappings:   m.KeyMappings,
			pathMappings:  m.PathMappings,
		}
		route.proxy = &httputil.ReverseProxy{
			FlushInterval:  -1, // SSE 场景下每次写入都立刻 flush
			Transport:      transport,
			Director:       makeDirector(route),
			ErrorHandler:   makeErrorHandler(m.Name),
			ModifyResponse: makeResponseLogger(m.Name),
		}

		routes[m.Name] = route
	}
	modelsList, err := buildModelsList(cfg.Models)
	if err != nil {
		return nil, fmt.Errorf("ai-proxy: build models list: %w", err)
	}
	return &proxyHandler{
		routes:          routes,
		modelsList:      modelsList,
		anthropicClient: &http.Client{Transport: transport},
	}, nil
}

// openAIModel 对应 OpenAI GET /v1/models 接口中单个 model 对象的最小字段集。
type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// openAIModelList 对应 GET /v1/models 的完整响应体。
type openAIModelList struct {
	Object string        `json:"object"`
	Data   []openAIModel `json:"data"`
}

// buildModelsList 按配置中 name 列表构造符合 OpenAI 规范的 /v1/models 响应体。
func buildModelsList(models []ModelConfig) ([]byte, error) {
	now := time.Now().Unix()
	list := openAIModelList{Object: "list"}
	for _, m := range models {
		list.Data = append(list.Data, openAIModel{
			ID:      m.Name,
			Object:  "model",
			Created: now,
			OwnedBy: "ai-proxy",
		})
	}
	return json.Marshal(list)
}

// sharedTransport 返回一个供所有模型复用的 http.Transport。
// 关键点：DisableCompression=true，避免默认 Transport 自动 gzip 解压导致的 SSE 缓冲。
func sharedTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableCompression = true
	return t
}

// ServeHTTP 处理 /v1/ 前缀的所有请求：解析 model → 改写 body → 反向代理。
// 实现 http.Handler 接口。
func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, span := concurrent.NewSpan(r.Context(), "ai-proxy: serve http")
	defer span.End()

	r = r.WithContext(ctx)
	start := time.Now()

	// 读取 body（对 GET 等无 body 的请求，ReadAll 立即返回空字节）。
	// 必须在此处读出，才能在日志和后续处理中共用。
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		log.New().Err(err).Text("读取请求 body 失败").ErrorContext(ctx)
		writeOpenAIError(w, http.StatusBadRequest, "read body: "+err.Error(), "invalid_request_error")
		return
	}
	// 还原 body，供后续逻辑读取。
	r.Body = io.NopCloser(bytes.NewReader(body))

	log.New().Text("收到请求").
		With("method", r.Method).
		With("uri", r.RequestURI).
		With("headers", r.Header).
		InfoContext(ctx)

	// Debug：收到请求时打印完整 body，便于流量分析。
	log.New().Text("收到请求 body").
		With("body", string(body)).
		DebugContext(ctx)

	// GET /v1/models：按 trpc_go.yaml 中 models[].name 构造 OpenAI 兼容的 model 列表。
	// id 为配置中的 name（客户端发 chat 时 model 字段应与此一致）；与上游真实 model 名可能不同。
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		log.New().Text("GET /v1/models").DebugContext(ctx)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(h.modelsList)
		return
	}

	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed,
			fmt.Sprintf("method %s not allowed", r.Method), "invalid_request_error")
		return
	}

	parsed, err := jsonvalue.Unmarshal(body)
	if err != nil {
		log.New().Err(err).Text("请求 body 不是合法 JSON").ErrorContext(ctx)
		writeOpenAIError(w, http.StatusBadRequest, "body is not valid JSON: "+err.Error(), "invalid_request_error")
		return
	}
	if !parsed.IsObject() {
		writeOpenAIError(w, http.StatusBadRequest, "body must be a JSON object", "invalid_request_error")
		return
	}

	modelName, err := parsed.GetString("model")
	if err != nil || modelName == "" {
		log.New().Err(err).Text("请求缺少 model 字段").WarnContext(ctx)
		writeOpenAIError(w, http.StatusBadRequest, `missing or invalid "model" field`, "invalid_request_error")
		return
	}

	route, ok := h.routes[modelName]
	if !ok {
		log.New().Text("model 未匹配到配置").With("model", modelName).WarnContext(ctx)
		writeOpenAIError(w, http.StatusBadRequest,
			fmt.Sprintf("model %q not found", modelName), "invalid_request_error")
		return
	}

	// 检查 path mapping
	targetPath := r.URL.Path
	var pathSetting *PathMappingConfigSetting
	if s, ok := matchPathMapping(route.pathMappings, r.URL.Path); ok {
		targetPath = s.Path
		pathSetting = &s
	}

	// Anthropic 转换分支：收到 OpenAI 协议请求，转换为 Anthropic 协议后转发，响应同样做反向转换。
	if pathSetting != nil && pathSetting.Anthropic {
		h.handleAnthropic(ctx, w, r, route, body, targetPath, start)
		return
	}

	// 非 Anthropic path mapping：直接重写 path（包括无 mapping 时的原样保留）。
	r.URL.Path = targetPath

	// 替换 model 字段为上游真实 model
	if _, err := parsed.Set(route.upstreamModel).At("model"); err != nil {
		log.New().Err(err).Text("替换 model 字段失败").ErrorContext(ctx)
		writeOpenAIError(w, http.StatusInternalServerError, "rewrite model: "+err.Error(), "internal_error")
		return
	}

	// 深度合并 extra（extra 优先）
	if route.extra != nil {
		deepMergeOverride(parsed, route.extra)
	}

	newBody, err := parsed.Marshal()
	if err != nil {
		log.New().Err(err).Text("序列化新 body 失败").ErrorContext(ctx)
		writeOpenAIError(w, http.StatusInternalServerError, "marshal body: "+err.Error(), "internal_error")
		return
	}

	stream, _ := parsed.GetBool("stream")

	// 计算最终上游 URL（仅用于日志，真实 URL 由 Director 构造）
	upstreamURL := route.scheme + "://" + route.host + r.URL.RequestURI()

	log.New().Text("转发请求").
		With("model", modelName).
		With("upstreamModel", route.upstreamModel).
		With("path", r.URL.Path).
		With("upstreamURL", upstreamURL).
		With("stream", stream).
		InfoContext(ctx)

	// 准备反向代理需要的字段：body / ContentLength / Header
	r.Body = io.NopCloser(bytes.NewReader(newBody))
	r.ContentLength = int64(len(newBody))
	r.Header.Set("Content-Length", fmt.Sprintf("%d", len(newBody)))
	// ReverseProxy 会自己管理这些字段，清掉以免冲突
	r.Header.Del("Content-Encoding")
	r.RequestURI = ""

	// 若配置了 key_mappings，则尝试替换 Authorization Bearer token
	applyKeyMapping(ctx, r, route.keyMappings)

	// Debug 级流量日志：完整请求头 + 改写后的 body。方便拦截和分析流量。
	log.New().Text("转发请求详情").
		With("model", modelName).
		With("upstreamModel", route.upstreamModel).
		With("path", r.URL.Path).
		With("upstreamURL", upstreamURL).
		With("stream", stream).
		With("reqHeaders", r.Header).
		With("reqBody", parsed).
		DebugContext(ctx)

	rec := &statusRecorder{ResponseWriter: w}
	route.proxy.ServeHTTP(rec, r)

	log.New().Text("转发完成").
		With("model", modelName).
		With("upstreamModel", route.upstreamModel).
		With("path", r.URL.Path).
		With("upstreamURL", upstreamURL).
		With("stream", stream).
		With("status", rec.status).
		With("cost", time.Since(start).Milliseconds()).
		InfoContext(ctx)
}

// makeDirector 生成 ReverseProxy 的 Director：
//   - 把请求的 scheme/host 重写为配置的上游
//   - path / rawQuery 保持客户端原样，原样转发到上游（例如 /v1/chat/completions、/v1/embeddings）
//   - header 原样透传（含 Authorization），Host 换成上游
func makeDirector(route *modelRoute) func(*http.Request) {
	return func(req *http.Request) {
		req.URL.Scheme = route.scheme
		req.URL.Host = route.host
		req.Host = route.host
	}
}

// makeErrorHandler 统一处理上游不可达等错误，避免把 trpc 默认错误页返回给客户端。
func makeErrorHandler(modelName string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		log.New().Err(err).Text("上游请求失败").
			With("model", modelName).
			With("url", r.URL.String()).
			ErrorContext(r.Context())
		writeOpenAIError(w, http.StatusBadGateway,
			fmt.Sprintf("upstream request failed: %v", err), "upstream_error")
	}
}

// makeResponseLogger 返回 ReverseProxy.ModifyResponse：
//   - 打印上游响应头（debug）
//   - 将 body 包一层 debugReadCloser，逐 chunk 输出 debug 日志
//
// 不会改动响应内容本身，也不会破坏 streaming（Read 直接透传）。
func makeResponseLogger(modelName string) func(*http.Response) error {
	return func(resp *http.Response) error {
		ctx := resp.Request.Context()

		log.New().Text("上游响应头").
			With("model", modelName).
			With("status", resp.StatusCode).
			With("rspHeaders", resp.Header).
			DebugContext(ctx)

		if resp.Body != nil {
			resp.Body = &debugReadCloser{
				inner: resp.Body,
				ctx:   ctx,
				model: modelName,
			}
		}
		return nil
	}
}

// debugReadCloser 包一层 io.ReadCloser，每次 Read 的数据都打 debug 日志。
// 为了保留 SSE 的 streaming 特性，不做任何缓冲，Read 原样透传。
type debugReadCloser struct {
	inner io.ReadCloser
	ctx   context.Context
	model string
	seq   int
}

func (d *debugReadCloser) Read(p []byte) (int, error) {
	n, err := d.inner.Read(p)
	if n > 0 {
		d.seq++
		log.New().Text("上游响应 chunk").
			With("model", d.model).
			With("seq", d.seq).
			With("size", n).
			With("data", string(p[:n])).
			DebugContext(d.ctx)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		log.New().Err(err).Text("上游响应 chunk 错误").
			With("model", d.model).
			With("seq", d.seq).
			DebugContext(d.ctx)
	}
	return n, err
}

func (d *debugReadCloser) Close() error {
	return d.inner.Close()
}

// deepMergeOverride 把 override 深度合并到 base 上；冲突时以 override 为准。
// 仅在双方都是 object 时递归下钻，否则直接整键覆盖。base、override 均要求是 object。
func deepMergeOverride(base, override *jsonvalue.V) {
	if base == nil || override == nil {
		return
	}
	if !base.IsObject() || !override.IsObject() {
		return
	}
	override.RangeObjects(func(k string, v *jsonvalue.V) bool {
		if existing, err := base.Get(k); err == nil && existing.IsObject() && v.IsObject() {
			deepMergeOverride(existing, v)
			return true
		}
		_, _ = base.Set(v).At(k)
		return true
	})
}

// statusRecorder 包装 http.ResponseWriter 以记录 status code，同时透传 Flush。
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.status = http.StatusOK
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush 让 ReverseProxy 的 FlushInterval=-1 逻辑能穿透到底层。
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// applyKeyMapping 检查请求的 Authorization 头，如果是 "Bearer <token>" 格式且 token 命中了
// mappings，则将 token 替换为映射后的值。未命中时保持原样，mappings 为空时直接返回。
func applyKeyMapping(ctx context.Context, r *http.Request, mappings map[string]string) {
	if len(mappings) == 0 {
		return
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return
	}
	token := auth[len(prefix):]
	if mapped, ok := mappings[token]; ok {
		r.Header.Set("Authorization", prefix+mapped)
		log.New().Text("替换 Authorization Bearer token").
			With("token", token).
			With("mapped", mapped).
			InfoContext(ctx)
	}
}

// openAIErrorBody 是 OpenAI 风格的错误响应体。
type openAIErrorBody struct {
	Error openAIErrorPayload `json:"error"`
}

type openAIErrorPayload struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
}

// writeOpenAIError 以 OpenAI 风格 JSON 返回错误。调用方不应再写响应。
func writeOpenAIError(w http.ResponseWriter, status int, message, typ string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	b, _ := json.Marshal(openAIErrorBody{
		Error: openAIErrorPayload{Message: message, Type: typ},
	})
	_, _ = w.Write(b)
}

// matchPathMapping 在 mappings 中查找与 path 匹配的项。
// 分别尝试 path 本身以及末尾添加/去掉斜杠的形式，以兼容两种配置写法。
// Path 字段为空的项视为未配置，跳过。
func matchPathMapping(mappings map[string]PathMappingConfigSetting, path string) (PathMappingConfigSetting, bool) {
	if len(mappings) == 0 {
		return PathMappingConfigSetting{}, false
	}

	if v, ok := mappings[path]; ok && v.Path != "" {
		return v, true
	}

	if strings.HasSuffix(path, "/") {
		stripped := strings.TrimRight(path, "/")
		if v, ok := mappings[stripped]; ok && v.Path != "" {
			return v, true
		}
	} else {
		if v, ok := mappings[path+"/"]; ok && v.Path != "" {
			return v, true
		}
	}

	return PathMappingConfigSetting{}, false
}

// extractBearerToken 从 Authorization: Bearer <token> 中提取 token 部分。
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return auth[len(prefix):]
}
