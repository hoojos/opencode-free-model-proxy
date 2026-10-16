# OpenCode OpenAI bridge

独立 Go 服务，无第三方依赖。将 OpenCode Zen 接口桥接为 OpenAI 客户端可用的接口，支持免费 DeepSeek 的流式和非流式聊天。

```bash
cd tools/opencode-bridge
BRIDGE_API_KEY='替换为你自己的密钥' GOPROXY=https://goproxy.cn go run .
```

默认监听 `127.0.0.1:8080`。OpenAI 客户端配置 base URL 为 `http://127.0.0.1:8080/v1`，API key 填 `BRIDGE_API_KEY` 配置的值。

`BRIDGE_API_KEY` 是必填的本地鉴权配置，未设置时服务拒绝启动。客户端通过 `Authorization: Bearer <密钥>` 传入。所有接口（包括模型列表）都会验证；未携带或密钥错误时返回 HTTP 401。修改密钥后需要重启桥接。

```bash
curl -sS http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer 替换为你自己的密钥' \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"你好"}],"stream":false}'
```

API key 只用于客户端到桥接的鉴权。桥接调用 OpenCode 时不发送 API key：会移除 `Authorization`、`X-Api-Key`、`X-Goog-Api-Key` 和 Cookie，也不添加 `Bearer public`。不需要配置上游 API key。

```bash
curl -sS http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer 替换为你自己的密钥'
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer 替换为你自己的密钥' \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"你好"}],"stream":true}'

# 非流式调用：桥接将上游 SSE 汇总为 chat.completion JSON
curl -sS http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer 替换为你自己的密钥' \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"你好"}],"stream":false}'
```

支持 `GET /v1/models`、`POST /v1/chat/completions`、`POST /v1/responses`。模型目录直接来自上游，不过滤为免费模型；模型可用性与额度由上游决定。`/v1/responses` 仅透传，不会转换成 Chat Completions；DeepSeek 请使用 `/v1/chat/completions`。

匿名调用上游时发送 OpenCode 1.x 的版本、客户端、项目、会话及请求标识。会话和请求 ID 按 1.19.1 的时间戳编码格式生成，不能用任意随机字符串替代。调用方可通过 `X-Opencode-Session` 传入有效的 OpenCode 会话 ID，连续请求默认各自生成 ID。

对于模型 ID 以 `-free` 结尾的 Chat Completions 请求：

- 上游请求使用 `stream: true`。调用方请求非流式时，汇总文本、推理内容、工具调用及 usage 后返回 JSON；请求流式时直接转发 SSE。
- 普通聊天没有工具时，补充 `read/edit/write` 声明，并强制 `tool_choice: "none"`。这些声明仅用于请求兼容，桥接不会读取、修改文件或执行工具。
- 调用方提供工具时，保留原工具和 `tool_choice`；上游仍会校验工具组合，自定义工具列表不保证满足免费接口要求。工具调用由调用方执行。
- 不覆盖系统提示词。上游 403、429 等错误和限流响应头原样返回；非流式汇总遇到中断或非法 SSE 时返回 502。

其他模型的请求体保持透传，是否允许匿名调用由上游决定。

| 环境变量 | 默认值 | 用途 |
| --- | --- | --- |
| `LISTEN_ADDR` | `127.0.0.1:8080` | 监听地址 |
| `OPENCODE_BASE_URL` | `https://opencode.ai/zen/v1` | 上游基础地址 |
| `OPENCODE_CLIENT_VERSION` | `1.19.1` | OpenCode 兼容版本标识 |
| `BRIDGE_API_KEY` | 无，必填 | 客户端调用本服务的 Bearer 密钥，仅用于本地鉴权 |
| `HTTPS_PROXY` | 空 | Go 标准 HTTP 传输支持的代理地址 |

如需局域网访问，设置 `LISTEN_ADDR=0.0.0.0:8080`。兼容规则基于 OpenCode 1.19.1 请求及 2026-10-16 的接口实测，上游规则变化时可能需要更新；版本标识不改变上游额度。

构建与静态检查：

```bash
GOPROXY=https://goproxy.cn go build -o /tmp/opencode-bridge .
GOPROXY=https://goproxy.cn go vet ./...
GOPROXY=https://goproxy.cn go test -race ./...
```
