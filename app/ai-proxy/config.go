package main

import (
	"github.com/Andrew-M-C/trpc-go-utils/plugin"
)

const (
	pluginType = "ai-proxy"
	pluginName = "default"
)

// Config 对应 trpc_go.yaml 中 plugins.ai-proxy.default 段。
type Config struct {
	Models []ModelConfig `yaml:"models"`
}

// ModelConfig 描述一个可被本服务路由到的模型。
type ModelConfig struct {
	// Name 是对外暴露的模型名，客户端请求 body 里 `model` 字段需等于此值。必须全局唯一。
	Name string `yaml:"name"`
	// Model 是真实转发给上游时使用的 `model` 值。
	Model string `yaml:"model"`
	// Host 是上游地址（scheme + host[:port]，不含 path）。
	// 例如 "https://tokenhub.tencentmaas.com"。客户端请求的完整 path 会原样拼到该 host 后面。
	Host string `yaml:"host"`

	// Extra 是附加到请求 body 的 JSON 字符串，采用深度合并，且优先级高于客户端。
	Extra string `yaml:"extra,omitempty"`
	// KeyMappings 是 key 映射，用于将客户端请求的 key 映射到上游请求的 key。
	KeyMappings map[string]string `yaml:"key_mappings,omitempty"`

	// PathMappings 是 path 映射，用于将客户端请求的 path 映射到上游请求的 path。
	PathMappings map[string]PathMappingConfigSetting `yaml:"path_mappings,omitempty"`
}

type PathMappingConfigSetting struct {
	// Path 是客户端请求的 path，用于将客户端请求的 path 映射到上游请求的 path。
	Path string `yaml:"path,omitempty"`
	// Anthropic 表示是否转换为 anthropic AI 协议
	Anthropic bool `yaml:"anthropic,omitempty"`
}

// bindConfig 将配置绑定到给定指针，需在 trpc.NewServer() 之前调用。
func bindConfig(target *Config) {
	plugin.Bind(pluginType, pluginName, target)
}
