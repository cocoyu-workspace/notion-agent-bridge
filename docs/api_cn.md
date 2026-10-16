# API 接入

[← 返回 README](../README.md)

`/v1/messages` 接收 Anthropic Messages API 请求体，是**唯一**对外的客户端入口。所有 admin/dashboard 接口都在 `/admin/*` 与 `/dashboard/` 下，使用独立的会话 cookie 鉴权（详见 [Dashboard](dashboard_cn.md)）。

## 鉴权

像调用 Anthropic 一样发送 API key：

```bash
-H "Authorization: Bearer <api_key>"
# 或
-H "x-api-key: <api_key>"
```

服务端 key 来自 `config.yaml` 的 `proxy.api_key`（环境变量 `PROXY_API_KEY` 可覆盖）。

## 基本请求

```bash
curl http://localhost:3000/v1/messages \
  -H "Authorization: Bearer <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "opus-4.6",
    "max_tokens": 1024,
    "messages": [
      { "role": "user", "content": "请总结一下 notion-manager 的用途。" }
    ]
  }'
```

如果不传 `model`，会自动使用 `proxy.default_model`。

## 请求头

| Header | 作用 |
| --- | --- |
| `Authorization: Bearer <key>` / `x-api-key: <key>` | API Key 鉴权（二选一，必传） |
| `Content-Type: application/json` | 必传 |
| `Accept: text/event-stream` | 可选；配合 `"stream": true` 使用，返回 Anthropic 标准 SSE 流 |
| `X-Web-Search: true|false` | 单次覆盖 Web 搜索开关（默认值来自 `proxy.enable_web_search`） |
| `X-Workspace-Search: true|false` | 单次覆盖 Workspace 搜索开关（默认值来自 `proxy.enable_workspace_search`） |

未识别的 header 会被忽略。

## ASK 模式（只读对话）

Notion 前端有一个 **Ask** 开关，开启后模型只能回答、不会编辑页面、也不会调用 tool。notion-manager 暴露两种触发方式：

1. **全局默认**：在 `config.yaml` 中 `proxy.ask_mode_default: true`，或在 Dashboard `Settings` 面板里实时切换。
2. **请求级后缀**：在模型名末尾追加 `-ask`，server 端在模型解析前会剥掉这个后缀，因此任何别名都能用：

```bash
# 即便全局默认是 off，这一次也强制只读
curl http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4.6-ask",
    "messages": [
      { "role": "user", "content": "总结 workspace 但不要做任何修改。" }
    ]
  }'
```

判定逻辑是 `OR`：只要后缀或全局默认任一为真，本次请求就走 ASK 模式。**`-ask` 不能反过来关闭全局默认**，如果想强制关闭只能取消 `ask_mode_default`。

## 搜索控制

全局开关由 `config.yaml` 与 Dashboard 管理，单次覆盖使用上面表格中的两个 header。示例：

```bash
curl http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -H "X-Web-Search: true" \
  -H "X-Workspace-Search: false" \
  -d '{
    "model": "sonnet-4.6",
    "messages": [
      { "role": "user", "content": "搜索最近关于 Go 1.25 的信息。" }
    ]
  }'
```

## 文件上传

支持以下媒体类型：

- `image/png`
- `image/jpeg`
- `image/gif`
- `image/webp`
- `application/pdf`
- `text/csv`

示例：

```bash
curl http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "sonnet-4.6",
    "max_tokens": 600,
    "messages": [{
      "role": "user",
      "content": [
        {
          "type": "document",
          "source": {
            "type": "base64",
            "media_type": "application/pdf",
            "data": "<base64>"
          }
        },
        {
          "type": "text",
          "text": "总结这份 PDF。"
        }
      ]
    }]
  }'
```

文件会由代理自动完成上传、轮询处理和转录注入。

## 研究模式

研究模式由模型名触发：

- `researcher`
- `fast-researcher`

示例：

```bash
curl -N http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "researcher",
    "stream": true,
    "max_tokens": 16000,
    "thinking": { "type": "enabled", "budget_tokens": 50000 },
    "messages": [
      { "role": "user", "content": "梳理一下 2026 年前后 Notion AI 代理工具常见架构。" }
    ]
  }'
```

研究模式注意事项：

- 只使用最后一条用户消息，属于单轮研究
- 会忽略文件上传
- 会忽略 `tools`
- 超时使用更长的 `proxy.research_timeout`（默认 `360s`）

## 流式响应

请求体里 `"stream": true`，会得到 Anthropic 标准 SSE 序列（`message_start`、`content_block_*`、`message_delta`、`message_stop`）。最后一个 delta 还会带 `usage_stats`，内含本次请求记账到对应账号的输入 / 输出 token，便于客户端做配额预测。

## 错误格式

完全遵循 Anthropic：

```json
{
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "message": "messages is required"
  }
}
```

常见错误：

- `401 unauthorized` —— API Key 缺失或错误
- `400 invalid_request_error` —— JSON 错误、`messages` 为空、附件类型不支持
- `429 rate_limit_error` —— 池内所有账号都已耗尽配额，错误消息会指明具体哪一项配额触发
- `502 bad_gateway` —— 上游 Notion 在重试后仍报错
