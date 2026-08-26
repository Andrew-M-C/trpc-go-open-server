# ai-proxy AI 流量转发器

基于 `trpc-go` 泛 HTTP 实现的 OpenAI 兼容流量转发器。

## 功能概述

- 监听 `/v1/chat/completions` 的 POST 请求（兼容 OpenAI SSE 协议标准）。
- 从请求 body 中解析 `model` 字段，按配置匹配后替换为目标上游的真实 `model`，并转发到对应 `uri`。
- 支持在配置中声明 `extra`，实现对请求 body 的深度合并，以便统一附加上游所需参数（例如 `thinking` 开关等）。
- 上游响应（状态码、header、body）原样透传回客户端；SSE 场景下实时 flush。

## 目录 / 文件约定

- 入口文件：`ai_proxy_main.go`
- 结构尽量简单扁平，**不使用依赖注入（wire）**。建议拆分如：
  - `ai_proxy_main.go`：`main` 入口。
  - `config.go`：插件配置结构与 `plugin.Bind` 注册。
  - `handler.go`：`/v1/chat/completions` 的处理逻辑。
  - 其他按需拆分即可。

## 配置

使用 `github.com/Andrew-M-C/trpc-go-utils/plugin` 解析，插件格式遵循 trpc 标准的 `plugins: <typ>: <name>: <config>` 三层结构。本服务约定：

- `typ`：`ai-proxy`
- `name`：`default`

```yaml
plugins:
  ai-proxy:
    default:
      models:
        - name: glm-5.1
          model: glm-5.1
          uri: "https://tokenhub.tencentmaas.com/v1/chat/completions"

        - name: deepseek-v3.2-thinking
          model: deepseek-v3.2
          uri: "https://tokenhub.tencentmaas.com/v1/chat/completions"
          extra: '{"thinking":{"type":"enabled"}}'
```

### 字段说明

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `name` | string | 是 | 对外暴露的模型名。客户端请求 body 里 `"model"` 字段需等于该值，用于路由匹配。**必须全局唯一**，重复时服务启动失败。 |
| `model` | string | 是 | 真实转发给上游时使用的 `model` 值；转发前会把请求 body 里的 `"model"` 替换为此值。 |
| `uri` | string | 是 | 完整上游 URL（含 path）。本服务不会再拼路径，直接 POST 到该 URL。 |
| `extra` | string | 否 | 一段 **JSON 字符串**，承载附加到请求 body 的 JSON 对象；采用**深度合并**，且 `extra` 优先级更高（冲突时以 `extra` 为准）。 |

> `extra` 特意使用 JSON 字符串而不是 YAML 原生结构：YAML 在标量上无法稳定区分 `true` / `"true"`、`1` / `"1"` 等类型，而上游对参数类型敏感，因此用 JSON 字符串精确表达。实现上先 `jsonvalue.Unmarshal(extra)`，再与客户端请求 body 做深度合并。

### `extra` 合并策略

- 使用 `github.com/Andrew-M-C/go.jsonvalue` 做**深度 merge**（对 object 逐层合并；非 object 值直接用 `extra` 覆盖客户端的值）。
- **`extra` 优先级高于客户端请求**：冲突时以 `extra` 为准。
- 举例：
  - 客户端：`{"thinking": {"type": "disabled", "budget": 1024}, "temperature": 0.7}`
  - `extra`：`{"thinking": {"type": "enabled"}, "temperature": 0.2}`
  - 合并结果：`{"thinking": {"type": "enabled", "budget": 1024}, "temperature": 0.2}`

### `trpc_go.yaml` 示例

```yaml
server:
  service:
    - name: trpc.amc.aiproxy.Proxy
      ip: 0.0.0.0
      port: 8088
      network: tcp
      protocol: http
      timeout: 0   # SSE 长连接，不做超时限制

plugins:
  log:
    default:
      - writer: console
        level: debug
      - writer: file
        level: info
        writer_config:
          filename: ./log.log
          max_size: 10
          max_backups: 10
          max_age: 7
          compress: false

  ai-proxy:
    default:
      models:
        - name: glm-5.1
          model: glm-5.1
          uri: "https://tokenhub.tencentmaas.com/v1/chat/completions"

        - name: deepseek-v3.2-thinking
          model: deepseek-v3.2
          uri: "https://tokenhub.tencentmaas.com/v1/chat/completions"
          extra: '{"thinking":{"type":"enabled"}}'
```

## HTTP 服务

- 使用 `trpc-go` 泛 HTTP：`RegisterNoProtocolServiceMux` + `http.ServeMux` 挂 `/v1/` 前缀。
- `GET /v1/models`：按配置中各条目的 `name` 返回 OpenAI 兼容的 model 列表（`id` 为 `name`，与直连上游是否实现该接口无关）。
- `POST /v1/...`：按路径与 `model` 字段转发（如 `POST /v1/chat/completions`）；`POST /v1/embeddings` 等只要 body 带 `model` 亦可走同一路由。
- 不暴露健康检查等额外路径。
- 不对调用方做鉴权，请求里 `Authorization` 等 header **原样透传** 给上游。

## 请求处理流程

1. 读取完整请求 body（JSON）。
2. 解析出 `model` 字段，在配置列表中匹配 `name`：
   - 找不到：返回错误（建议 `400` + OpenAI 风格错误 JSON，或参考上游通用错误格式；实现时按简单 400 + `{"error":{"message":"model not found"}}` 即可）。
   - 找到：将 body 中的 `"model"` 替换为配置的 `model`。
3. 如果配置了 `extra`，对 body 做深度合并，`extra` 优先。
4. 构造新的上游请求：
   - 方法：POST
   - URL：配置中的 `uri`
   - Body：合并后的 JSON
   - Header：把客户端请求的 header **原样透传**（包括 `Authorization`）。Hop-by-hop header **无需过滤**。
5. 调用上游，把上游响应的 **状态码、header、body** 原样写回客户端。
6. 流式响应时，确保边读边 flush（见下文）。

## 响应与 Flush

- 响应状态码、header 原样透传。
- 同时支持 `stream: true`（SSE）和 `stream: false` / 缺省（一次性 JSON）两种场景。
- 允许使用 `httputil.ReverseProxy`，只要能满足原样透传 + SSE 即时 flush（设置 `FlushInterval: -1`）即可。
- 若不使用 `ReverseProxy`、而是手写 roundtrip，则循环 `io.Copy`/`Read` 小块数据后立即 `http.Flusher.Flush()`，不要做额外缓冲。

## 超时 / 取消 / 错误

- 不做额外的超时控制，全部透传（SSE 长连接需要）。
- 客户端断连 / 上游错误等情况，不做复杂处理；`ReverseProxy` 的默认行为足够。
- 不包装上游返回的错误，直接透传上游响应即可。
- 唯一例外：匹配不到 `model` 时由本服务直接返回错误响应（见上文）。

## 日志

按 `trpc-go-group` skill 的约定使用 `github.com/Andrew-M-C/trpc-go-utils/log` 记录请求日志，使用 `*Context` 系列方法以带上 trace。建议字段：

- `model`（客户端侧 name）
- `upstreamModel`（配置里的 `model`）
- `uri`（上游 URL）
- `stream`（bool，是否是 SSE）
- `status`（上游返回状态码）
- `cost`（耗时，单位 ms）

`Authorization` 等敏感信息不必单独脱敏，正常记录即可（本服务本身就不对鉴权做任何处理）。

## Anthropic 路径：Token 相关参数

当某条 `models[]` 配置了 `path_mappings` 且 `anthropic: true` 时，客户端仍走 **OpenAI** `POST /v1/chat/completions`，代理会转换为 Anthropic `POST /v1/messages` 再转发。下列参数均指**发往 Anthropic 上游**的 Messages API 请求体/响应体中的 token 字段（官方文档：[Messages API](https://docs.anthropic.com/en/api/messages)、[Extended thinking](https://docs.anthropic.com/en/docs/build-with-claude/extended-thinking)）。

### 在 ai-proxy 中如何生效

| 来源 | 说明 |
|------|------|
| 客户端 body | 除 `model` / `messages` / `tools` / `stream` 外的字段会作为 extra 合并进 Anthropic 请求（例如客户端传 `max_tokens`）。 |
| `extra` 配置 | 与客户端 extra 深度合并，**`extra` 优先**（冲突时覆盖客户端）。 |
| `anthropic.BuildRequest` | 若合并后仍无 `max_tokens` 或为 `0`，库会写入默认值 **`100000`**（见 `go.util/openai/anthropic/request.go`）。Anthropic 路径下代理**强制 `stream: true`**。 |

常见错误：把 `max_tokens` 写在 `thinking` 对象**内部**（无效字段）；或 `budget_tokens` ≥ `max_tokens`（上游可能 400，部分网关会长时间无响应）。

### 请求参数（限制生成与思考）

| 参数 | JSON 位置 | 含义 | 限制与说明 |
|------|-----------|------|------------|
| **`max_tokens`** | 请求体**顶层**（必填） | 单次回复最多生成的 token 上限（含思考 + 正文 + tool 输出，具体计费以 Anthropic 为准）。模型可能提前结束。 | 各模型有不同**上限**（见 [Models](https://docs.anthropic.com/en/docs/about-claude/models)）。须 **`max_tokens` > 0** 才能正常生成（`0` 仅用于 prompt cache 预热等特殊场景）。开启 **manual extended thinking**（`thinking.type: "enabled"`）时：**`max_tokens` 必须大于 `thinking.budget_tokens`**；且不能与 `max_tokens: 0` 组合做 cache 预热。 |
| **`thinking.budget_tokens`** | `thinking` 对象内 | Manual extended thinking 时，允许模型用于**内部推理**的 token 预算上限（完整 thinking token，非摘要）。 | 仅当 **`thinking.type` 为 `"enabled"`** 时有效。官方要求 **≥ 1024**。须满足 **`budget_tokens` < `max_tokens`**。thinking 消耗计入 **`max_tokens`** 额度。在 Opus 4.6 / Sonnet 4.6 上仍可用但已标记 deprecated；**Opus 4.7 不支持** manual `budget_tokens`（会 400）。与 **interleaved thinking + tools** 时，实际思考可能超过 `budget_tokens`，上限可为整窗 context（见官方 extended thinking 文档）。 |
| **`thinking.type`** | `thinking` 对象内 | 是否启用扩展思考及模式。 | `"disabled"`：默认，不思考。`"enabled"`：manual，需配 **`budget_tokens`**。`"adaptive"`（较新模型推荐）：由模型按需思考，用 **`output_config.effort`** 控制深度，**不要**再设 `budget_tokens`。 |
| **`output_config.effort`** | `output_config` 对象内 | Adaptive thinking 的推理深度档位。 | 在 **`thinking.type: "adaptive"`** 时使用。常见值：`low` / `medium` / `high`；部分模型支持 `xhigh`、`max`（以当前模型文档为准）。**不直接是 token 数字**，而是影响思考深度的枚举。 |

与 thinking 同时开启时的其它约束（Anthropic 官方）：

- 启用 extended thinking 时 **`temperature` 固定为 1.0**（若传其它值会被忽略或报错，以实现为准）。
- **`top_p` / `top_k`** 在 thinking 模式下通常不可与 manual thinking 同用。
- **`stop_sequences`** 与 extended thinking **不兼容**（会 400）。

OpenAI 客户端字段映射（经代理转换时）：

| OpenAI（客户端） | Anthropic（上游） |
|------------------|-------------------|
| `max_tokens` | `max_tokens`（顶层） |
| `stop`（字符串或数组） | `stop_sequences`（`BuildRequest` 自动映射） |
| `reasoning_content`（assistant 多轮） | 还原为 `thinking` / `redacted_thinking` block（含 `signature`） |

### 响应中的 Token 统计（`usage`）

流式场景下，token 用量多在 **`message_delta`** 事件的 `usage` 中出现；代理会映射为 OpenAI SSE 里的 `usage.prompt_tokens` / `completion_tokens` / `total_tokens`。

| 字段 | 位置 | 含义 |
|------|------|------|
| **`input_tokens`** | `usage` | 本次请求计入计费的输入 token（含 system、messages 等）。 |
| **`output_tokens`** | `usage` | 本次生成的输出 token（**含 thinking 与正文**等输出侧 token，按 Anthropic 计费规则）。 |
| **`cache_creation_input_tokens`** | `usage` | 写入 prompt cache 的 token 量（启用 cache 时）。 |
| **`cache_read_input_tokens`** | `usage` | 从 prompt cache 读取的 token 量。 |
| **`cache_creation.ephemeral_5m_input_tokens`** | `usage.cache_creation` | 5 分钟 TTL cache 写入量（细分项）。 |
| **`cache_creation.ephemeral_1h_input_tokens`** | `usage.cache_creation` | 1 小时 TTL cache 写入量（细分项）。 |

### 关联关系（配置时建议对照）

```text
                    ┌─────────────────────────────────────┐
                    │  max_tokens（顶层，必填/有默认值）     │
                    │  = 本次回复总生成上限                 │
                    └─────────────────────────────────────┘
                                      │
          ┌───────────────────────────┼───────────────────────────┐
          │                           │                           │
          ▼                           ▼                           ▼
   thinking 正文              tool 输出 JSON              普通 text
   （若启用）                  （若调用工具）

thinking.type = "enabled" 时：
  thinking.budget_tokens ≤ 推理预算（≥1024）
  且 budget_tokens < max_tokens

thinking.type = "adaptive" 时：
  用 output_config.effort 控制深度，不设 budget_tokens
```

**推荐配置示例**（`trpc_go.yaml` 的 `extra`，与当前 Claude Haiku 路由一致）：

```yaml
extra: '{"max_tokens":8192,"thinking":{"type":"enabled","budget_tokens":4096}}'
```

- `8192` > `4096`，满足 `max_tokens > budget_tokens`。
- 勿将 `max_tokens` 放进 `thinking` 内部。
- 若客户端不再传 `max_tokens`，仅靠 `extra` 即可；若客户端也传 `max_tokens`，未在 `extra` 中覆盖时以客户端为准。

### 上下文窗口（非请求字段，但会限制 token）

单次请求中 **输入（system + messages + tools 等）+ 输出（≤ `max_tokens`）** 不能超过该模型的 **context window**（例如 Haiku 4.5 常为 200K，以 [Models](https://docs.anthropic.com/en/docs/about-claude/models) 为准）。这与 `max_tokens`、`budget_tokens` 是不同层级的上限：前者管「整次对话能装多少」，后者管「这次最多生成多少」。

### 第三方网关注意

经 OneAPI / 自建网关转发时，可能对 **`max_tokens` 过大**做额度预扣或拒绝（例如返回 402 `insufficient_user_quota`），与 Anthropic 官方行为不一致。排错时可用**相同 JSON body** 直接 `curl` 上游 `/v1/messages` 对比。

## 调试命令

以下命令默认在仓库内 `app/ai-proxy` 目录执行，且与本目录下 `trpc_go.yaml` 中 `port: 8088` 一致；若你改了端口或配置，请替换 URL 与 `model` 名。

### 启动服务

```bash
cd app/ai-proxy
go run .
```

### 非流式对话（`stream` 缺省或 `false`）

```bash
curl -sS -X POST "http://127.0.0.1:8088/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -d '{
    "model": "glm-5.1",
    "messages": [{"role": "user", "content": "ping"}],
    "max_tokens": 16
  }' | jq .
```

### 流式 SSE（`stream: true`）

`-N`（`--no-buffer`）避免 curl 把 SSE 攒到结束再输出，便于边看边调。

```bash
curl -sS -N -X POST "http://127.0.0.1:8088/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -d '{
    "model": "glm-5.1",
    "messages": [{"role": "user", "content": "ping"}],
    "stream": true,
    "max_tokens": 64
  }'
```

### 验证 `extra` 合并（需配置里存在带 `extra` 的模型名）

```bash
curl -sS -N -X POST "http://127.0.0.1:8088/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -d '{
    "model": "deepseek-v3.2-thinking",
    "messages": [{"role": "user", "content": "hi"}],
    "stream": true
  }'
```

### 故意触发本机错误（`model` 未配置 → 400）

```bash
curl -sS -w "\nHTTP_CODE:%{http_code}\n" -X POST "http://127.0.0.1:8088/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model":"non-existent-model","messages":[]}'
```

### 看流量与 debug 日志

`trpc_go.yaml` 里若将 `log.default` 中某一 writer 的 `level` 设为 `debug`，控制台或 `./log.log` 会包含 `转发请求详情`、`上游响应头`、`上游响应 chunk` 等字段。

```bash
# 同目录下 tail 文件日志（路径与 yaml 里 filename 一致）
tail -f ./log.log
```

```bash
# 只关注本服务打的「转发 / 上游」关键字（按你本地 grep 习惯二选一）
grep -E '转发请求|上游响应' ./log.log
```

### 通过本服务抓取到的 Cursor 请求

- [cursor_prompt](./docs/cursor_prompt.md)
