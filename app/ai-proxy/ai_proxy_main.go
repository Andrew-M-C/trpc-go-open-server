// Package main 实现 ai-proxy：一个基于 trpc-go 泛 HTTP 的 OpenAI 兼容流量转发器。
// 详见同目录 README.md。
package main

import (
	"net/http"

	"github.com/Andrew-M-C/trpc-go-utils/log"
	"github.com/Andrew-M-C/trpc-go-utils/recovery"
	trpc "trpc.group/trpc-go/trpc-go"
	thttp "trpc.group/trpc-go/trpc-go/http"
)

const serviceName = "trpc.amc.aiproxy.Proxy"

func main() {
	defer recovery.CatchPanic(recovery.WithErrorLog())

	// 必须在 NewServer 之前绑定；NewServer 阶段会把 yaml 里的 plugins.ai-proxy.default 解码进 cfg。
	var cfg Config
	bindConfig(&cfg)

	svc := trpc.NewServer()

	h, err := newProxyHandler(&cfg)
	if err != nil {
		log.New().Err(err).Text("初始化 ai-proxy handler 失败").Fatal()
		return
	}

	// 使用 net/http ServeMux，"/v1/" 为前缀匹配，所有 /v1/* 的请求都走 proxyHandler。
	// 这样 /v1/chat/completions、/v1/embeddings 等 OpenAI 兼容路径都能透明转发。
	mux := http.NewServeMux()
	mux.Handle("/v1/", h)

	thttp.RegisterNoProtocolServiceMux(svc.Service(serviceName), mux)

	log.New().Format("ai-proxy listening (service=%s), POST /v1/*", serviceName).Info()

	if err := svc.Serve(); err != nil {
		log.New().Err(err).Text("服务运行失败").Error()
		return
	}

	log.Warn("server exit")
}
