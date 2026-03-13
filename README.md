# trpc-agent-demo

这个仓库提供了一个最小可运行的 demo：使用 `trpc-agent-go` 构建 `llmagent`，挂载 `calculator` 工具，并通过 AG-UI 兼容的 SSE 接口对外提供服务。

## Features

- `trpc-agent-go/agent/llmagent` 作为核心 Agent。
- `calculator` Function Tool 负责加减乘除计算。
- `runner` + 内存 session 负责按 `threadId` 维护多轮上下文。
- `POST /agent` 输出 AG-UI 事件流，可直接接入 AG-UI 客户端或 Dojo。
- `GET /healthz` 提供健康检查。

## Environment

```bash
export OPENAI_API_KEY=your-api-key
export OPENAI_BASE_URL=https://api.deepseek.com
export MODEL_NAME=deepseek-chat
export SERVER_ADDR=:8080
```

- `OPENAI_API_KEY` 必填。
- `OPENAI_BASE_URL` 选填，适用于 DeepSeek 或其他 OpenAI 兼容服务。
- `MODEL_NAME` 默认 `deepseek-chat`。
- `SERVER_ADDR` 默认 `:8080`。

## Run

```bash
go run .
```

启动后可用以下请求验证：

```bash
curl -N http://localhost:8080/agent \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -d '{
    "threadId": "thread-demo",
    "runId": "run-demo",
    "state": {},
    "messages": [
      {
        "id": "msg-1",
        "role": "user",
        "content": "Please use the calculator to compute 12 * 8."
      }
    ],
    "tools": [],
    "context": [],
    "forwardedProps": {}
  }'
```

服务会先输出 `RUN_STARTED`，再按需输出 `TEXT_MESSAGE_*`、`TOOL_CALL_*`、`TOOL_CALL_RESULT`，最后输出 `RUN_FINISHED`。

## Test

```bash
go test ./...
```
