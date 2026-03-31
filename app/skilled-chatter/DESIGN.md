# skilled-chatter 设计 / 实施清单

基于 `trpc.group/trpc-go/trpc-go` 与 `trpc.group/trpc-go/trpc-agent-go`：tRPC HTTP 挂载**极简 JSON 聊天**，底层为带 Skill 工具链的 `LLMAgent` + `runner.Run`（工具与 skill 在服务端自动跑完）。

## trpc-agent-go 里有哪些「现成 AI 协议」？

当前依赖的 **`trpc-agent-go` v1.7.0** 在 `server` 包下主要提供：

| 方式 | 包路径 | 说明 |
|------|--------|------|
| OpenAI 兼容 HTTP | `server/openai` | `/v1/chat/completions`，形态熟悉，但本质是「把一次 `Run` 的事件流折成 OpenAI JSON」，容易和「客户端自己跑 tool」的语义混在一起。 |
| A2A | `server/a2a` | Agent-to-Agent（依赖 `trpc-a2a-go`），适合 agent 互联，需配置对外 `host` / Agent Card 等，**接入代码量比「直接 Run」大**。 |
| README 中的 AG-UI 等 | 文档与 [上游示例](https://github.com/trpc-group/trpc-agent-go) | **不在** v1.7.0 发布模块的 `server` 树里；若要用需跟上游版本与示例对齐。 |

若目标是 **代码最少**、且 **Skill/工具全部由服务端闭环**，框架侧的核心能力是 **`runner.Run` + `LLMAgent`（`WithSkills`）**，不必绑 OpenAI 形态。本应用采用 **`POST /chat` + JSON**（见 [`http_chat.go`](./http_chat.go)），仅几十行，客户端只收最终 `reply` 字符串。

## 架构

- 客户端 → **`POST /chat`**（`{"message":"..."}`）→ `runner.Run` → `llmagent` → 模型 API + `skill_load` / `skill_run` 等。
- **多轮 tool 在进程内完成**：客户端无需处理 `tool_calls`。
- Skill 为磁盘目录下的 `SKILL.md`；调试 skill `current_time` 通过 `skill_run` 执行 `date`。
- **模型参数**（`base_url`、`api_key`、`model`）来自与本目录 [`trpc_go.yaml`](./trpc_go.yaml) 中 `plugins.model.openai`，通过 [`github.com/Andrew-M-C/trpc-go-utils/plugin`](https://github.com/Andrew-M-C/trpc-go-utils) 的 `plugin.Bind("model", "openai", &cfg)` 在 `trpc.NewServer()` 时注入。
- 进程内日志使用 [`github.com/Andrew-M-C/trpc-go-utils`](https://github.com/Andrew-M-C/trpc-go-utils) 的 `log` 包，与 `plugins.log` 的 tRPC 日志配合。

## Checklist

- [x] **依赖**：`go.mod` 含 `trpc-go`、`trpc-agent-go`、`trpc-go-utils/plugin`。
- [x] **Skill**：`skills/current_time/SKILL.md`（front matter + 说明用 `skill_run` + `date`）。
- [x] **main**：`plugin.Bind` → `trpc.NewServer()` → `openaimodel` / `llmagent` / `runner.NewRunner` → `newChatMux` → `RegisterNoProtocolServiceMux` → `Serve`。
- [x] **配置**：`app/skilled-chatter/trpc_go.yaml`（`server` + `plugins.model.openai`）；`trpc.ServerConfigPath` 指向该文件（与 `main.go` 同目录）。
- [x] **验证**：`go build ./app/skilled-chatter/`；监听端口见 yaml 中 `server.service[].port`（默认与代码中 `serviceName` 一致）。

## 运行说明

- 编辑 `trpc_go.yaml` 中 `plugins.model.openai`（`api_key`、`base_url`、`model`）。勿将真实 key 提交到公开仓库。
- 启动：在 `app/skilled-chatter` 下执行 `go run .`，或在仓库根执行 `go run ./app/skilled-chatter/`；默认读取与 `main.go` 同目录的 `trpc_go.yaml`。
- 可用 `-conf /path/to/trpc_go.yaml` 指定其它配置文件。

## 本地调试示例

```bash
cd app/skilled-chatter
go run .
```

HTTP 监听地址与端口以 [`trpc_go.yaml`](./trpc_go.yaml) 中 `server.service[0].ip` / `port` 为准（当前示例为 `0.0.0.0:8088`）。

**聊天**（单次请求内跑完全部 skill/tool，返回合成后的 assistant 文本）：

```bash
curl -sS -X POST 'http://127.0.0.1:8088/chat' \
  -H 'Content-Type: application/json' \
  -d '{"message":"现在几点？请用本机 date 说明。"}'
```

响应形如：`{"reply":"...","session_id":"..."}`（未带 `X-Session-ID` 时服务端会生成 `session_id`，下次可原样带回以续聊）。

`reply` 取**最后一次**满足 `model.Response.IsFinalResponse()` 的 assistant `Message.Content`（即排除 tool-call 中间事件）；若无则退回拼接全部流式 `Delta.Content`。修改聚合逻辑后需**重新编译并重启**进程。

可选请求头：

- **`X-Session-ID`**：会话 id（与此前 OpenAI 示例里用法一致）。
- **`X-User-ID`**：用户 id（默认 `default`）。

### 为何 curl 很快结束、没有正常 reply？

常见原因：

1. **`server.service[].timeout` 过小（最常见）**  
   单位为**毫秒**。多轮模型 + `skill_run` 需要较长时间，请使用如 `300000`（5 分钟）等，**改完后需重启**。

2. **`plugins.model.openai` 未配置或 key 无效**  
   查看 `log.log` 或提高 console 日志 `level`。

3. **`curl` 未加 `-sS`**  
   出错时可能看不到错误信息。

## 安全提示

`skill_run` 默认在本地 executor 中执行 shell，仅适合调试；生产应使用命令白名单或隔离执行环境。
