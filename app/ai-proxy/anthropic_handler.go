package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	jsonvalue "github.com/Andrew-M-C/go.jsonvalue"
	"github.com/Andrew-M-C/go.util/openai/anthropic"
	"github.com/Andrew-M-C/trpc-go-utils/log"
	openai "github.com/sashabaranov/go-openai"
)

// anthropicReservedKeys 是已由 BuildRequest 单独处理、不应作为 extra field 透传的客户端字段。
var anthropicReservedKeys = map[string]struct{}{
	"model":    {},
	"messages": {},
	"tools":    {},
	"stream":   {},
}

// buildAnthropicExtraFields 从客户端 body 中提取除 reserved key 外的所有字段作为 extra fields，
// 再用 routeExtra 覆盖合并（routeExtra 优先），返回可直接传给 anthropic.BuildRequest 的对象。
func buildAnthropicExtraFields(body []byte, routeExtra *jsonvalue.V) (*jsonvalue.V, error) {
	parsed, err := jsonvalue.Unmarshal(body)
	if err != nil {
		return nil, err
	}

	extra := jsonvalue.NewObject()
	parsed.RangeObjects(func(key string, value *jsonvalue.V) bool {
		if _, reserved := anthropicReservedKeys[key]; reserved {
			return true
		}
		_, _ = extra.Set(value).At(key)
		return true
	})

	if routeExtra != nil {
		deepMergeOverride(extra, routeExtra)
	}
	return extra, nil
}

// handleAnthropic 处理命中了 anthropic:true path mapping 的请求。
//
// 上行：将客户端的 OpenAI 协议 body 转换为 Anthropic Messages 协议，直接 POST 到上游。
// 下行：将上游的 Anthropic SSE 流通过 anthropic.NewSSEReader 转换为 OpenAI SSE 格式，写回客户端。
//
// 该链路不经过 httputil.ReverseProxy，是独立的处理分支。
func (h *proxyHandler) handleAnthropic(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	route *modelRoute,
	body []byte,
	targetPath string,
	start time.Time,
) {
	// key mapping 后提取 Bearer token 作为上游的 x-api-key
	applyKeyMapping(ctx, r, route.keyMappings)
	apiKey := extractBearerToken(r)

	// 解析请求 body，取出 messages 和 tools
	var req struct {
		Messages []openai.ChatCompletionMessage `json:"messages"`
		Tools    []openai.Tool                  `json:"tools,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		log.New().Err(err).Text("Anthropic: 解析请求 body 失败").ErrorContext(ctx)
		writeOpenAIError(w, http.StatusBadRequest, "parse body: "+err.Error(), "invalid_request_error")
		return
	}

	// 收集客户端 body 中的额外参数（max_tokens、temperature、top_p、stop 等），
	// 再用 route.extra 覆盖合并（route.extra 优先），一并交给 BuildRequest 处理。
	// 否则客户端传入的 max_tokens 等参数会被丢弃，BuildRequest 退化为默认 max_tokens。
	extraFields, err := buildAnthropicExtraFields(body, route.extra)
	if err != nil {
		log.New().Err(err).Text("Anthropic: 解析额外参数失败").ErrorContext(ctx)
		writeOpenAIError(w, http.StatusBadRequest, "parse extra fields: "+err.Error(), "invalid_request_error")
		return
	}

	// 将 OpenAI 请求转换为 Anthropic 格式；extraFields 由 BuildRequest 内部处理，
	// 支持 stop → stop_sequences 等字段映射。
	anthropicBody, err := anthropic.BuildRequest(route.upstreamModel, req.Messages, req.Tools, extraFields)
	if err != nil {
		log.New().Err(err).Text("Anthropic: 构建请求失败").ErrorContext(ctx)
		writeOpenAIError(w, http.StatusInternalServerError, "build anthropic request: "+err.Error(), "internal_error")
		return
	}

	upstreamURL := fmt.Sprintf("%s://%s%s", route.scheme, route.host, targetPath)

	log.New().Text("转发 Anthropic 请求").
		With("model", route.name).
		With("upstreamModel", route.upstreamModel).
		With("path", r.URL.Path).
		With("targetPath", targetPath).
		With("upstreamURL", upstreamURL).
		InfoContext(ctx)

	log.New().Text("转发 Anthropic 请求详情").
		With("model", route.name).
		With("reqBody", string(anthropicBody)).
		DebugContext(ctx)

	// 向上游发起 POST 请求，使用与代理相同的 Transport（DisableCompression=true，适配 SSE）
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(anthropicBody))
	if err != nil {
		log.New().Err(err).Text("Anthropic: 构建 HTTP 请求失败").ErrorContext(ctx)
		writeOpenAIError(w, http.StatusInternalServerError, "create upstream request: "+err.Error(), "internal_error")
		return
	}
	httpReq.Header = http.Header{
		"Content-Type":      {"application/json"},
		"anthropic-version": {"2023-06-01"},
	}
	if apiKey != "" {
		httpReq.Header.Set("x-api-key", apiKey)
	}

	resp, err := h.anthropicClient.Do(httpReq)
	if err != nil {
		log.New().Err(err).Text("Anthropic: 上游请求失败").
			With("model", route.name).
			With("url", upstreamURL).
			ErrorContext(ctx)
		writeOpenAIError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "upstream_error")
		return
	}
	defer resp.Body.Close()

	log.New().Text("Anthropic 上游响应头").
		With("model", route.name).
		With("status", resp.StatusCode).
		With("rspHeaders", resp.Header).
		DebugContext(ctx)

	// 非 200：解析 Anthropic 错误体并转换为 OpenAI 格式
	if resp.StatusCode != http.StatusOK {
		writeAnthropicError(w, resp, ctx, route.name)
		return
	}

	// 将 Anthropic SSE 流转换为 OpenAI SSE 格式，写回客户端
	sseBody := anthropic.NewSSEReader(resp.Body)
	defer sseBody.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	seq := 0
	for {
		n, readErr := sseBody.Read(buf)
		if n > 0 {
			seq++
			log.New().Text("Anthropic 响应 chunk").
				With("model", route.name).
				With("seq", seq).
				With("size", n).
				With("data", string(buf[:n])).
				DebugContext(ctx)
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				log.New().Err(writeErr).Text("Anthropic: 写响应失败").
					With("model", route.name).
					ErrorContext(ctx)
				break
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.New().Err(readErr).Text("Anthropic: 读取 SSE 流失败").
					With("model", route.name).
					ErrorContext(ctx)
			}
			break
		}
	}

	log.New().Text("Anthropic 转发完成").
		With("model", route.name).
		With("upstreamModel", route.upstreamModel).
		With("upstreamURL", upstreamURL).
		With("cost", time.Since(start).Milliseconds()).
		InfoContext(ctx)
}

// writeAnthropicError 读取 Anthropic 非 200 响应体，将错误信息转换为 OpenAI 格式返回给客户端。
func writeAnthropicError(w http.ResponseWriter, resp *http.Response, ctx context.Context, modelName string) {
	body, _ := io.ReadAll(resp.Body)

	log.New().Text("Anthropic 上游返回错误").
		With("model", modelName).
		With("status", resp.StatusCode).
		With("body", string(body)).
		ErrorContext(ctx)

	// Anthropic 错误格式：{"type":"error","error":{"type":"...","message":"..."}}
	var errBody struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if jsonErr := json.Unmarshal(body, &errBody); jsonErr == nil && errBody.Error.Message != "" {
		writeOpenAIError(w, resp.StatusCode, errBody.Error.Message, errBody.Error.Type)
		return
	}

	writeOpenAIError(w, resp.StatusCode, string(body), "upstream_error")
}
