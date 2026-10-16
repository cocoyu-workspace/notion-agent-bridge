# Personal Agent 传输协议与会话绑定（Agent Thread Protocol）

本文档说明 `notion-code-bridge` 的 Notion Personal Agent 传输协议（`proxy.agent_thread: true`）、模型矩阵、跨轮会话绑定（`.agent_threads.state`）、权限映射与 `claude-notion.py` 启动器机制。

---

## 1. 配置与启用

在 `config.yaml` 中启用 Personal Agent 传输：

```yaml
proxy:
  agent_thread: true
  notion_api_base: https://app.notion.com/api/v3
  default_model: sonnet-5.5
```

支持环境变量覆盖：`AGENT_THREAD=true`、`NOTION_API_BASE=https://app.notion.com/api/v3`、`DEFAULT_MODEL=sonnet-5.5`。

- **首轮创建线程（`createAgentThread`）**：客户端会话的第 1 轮创建 Notion Agent 线程，发送完整的系统提示词（含 `LOCAL ACTION PROTOCOL`）与首轮用户消息。
- **后续轮次续聊（`sendEventToAgentThread`）**：同一客户端会话的后续轮次复用已绑定的 Notion 线程，仅发送新增的用户消息或本地工具执行结果（`ns[rec.ConvLen:]`），只返回序号大于上一轮基线（`baseline`）的新实体。
- **本地工具 JSON 桥接**：Notion 端禁用网页浏览、建议编辑与 Notion 记忆，所有本地文件读写、搜索、Shell 命令与本地子 Agent 均以 JSON 动作格式返回给客户端本地执行。

---

## 2. 模型矩阵、`/model` 交互菜单与推理强度

| `/model` 菜单显示名称 | 启动参数 / 模型别名 | Notion 上游模型 ID | 服务端上下文窗口 | 本地启动器与桌面端窗口配置 | 默认推理强度 |
| --- | --- | --- | --- | --- | --- |
| **Sonnet 5.5（Notion）**（默认） | `sonnet-5.5` / `claude-sonnet-5-5` / `achira-donut` | `achira-donut` | **200,000（200k）** | `CLAUDE_CODE_AUTO_COMPACT_WINDOW=200000`<br>`modelSettings.*.autoCompactWindow=200000` | 主会话 `low`，本地子 Agent 自动提升为 `high` |
| **Opus 5.5（Notion）** | `opus-5.5` / `claude-opus-5-5` / `albuquerque-quinn` | `albuquerque-quinn` | **200,000（200k）** | `CLAUDE_CODE_AUTO_COMPACT_WINDOW=200000`<br>`modelSettings.*.autoCompactWindow=200000` | `medium`（可通过 `--effort` 覆盖） |
| **GPT-6.1 Sol（Notion）** | `gpt-6.1-sol` / `gpt-6.1-sol[1m]` / `claude-sol-6-1[1m]` / `omniberry-sundae` | `omniberry-sundae` | **1,000,000（1M）** | `CLAUDE_CODE_MAX_CONTEXT_TOKENS=1000000`<br>`CLAUDE_CODE_AUTO_COMPACT_WINDOW=1000000` | `medium`（可通过 `--effort` 覆盖） |
| *（仅命令行 `--model auto`）* | `auto` / `claude-auto` | `<不传 model 字段>` | 随服务端所选模型而定 | `CLAUDE_CODE_AUTO_COMPACT_WINDOW=200000` | `<unset>`（随 `--effort` 透传） |

- **严格拒绝未知模型**：除上述受支持模型（及本地子 Agent 请求中的 `haiku` 自动映射为 `sonnet-5.5` + `high`）外，任何未知模型名均在建线程前直接返回 `400 [bridge:model_unavailable]`，绝不静默回退。
- **无冗余 `provider_handoff` 状态实体**：未切换模型的后续跟进轮次省略 `model` 字段（仅透传 `reasoningEffort`），避免 Notion 聊天记录每轮刷出「已将模型更改为...」状态提示；当会话中途通过 `/model` 切换模型时，`turn.followUpModel` 在下一轮跟进请求中携带一次新 `model` 并更新绑定。

---

## 3. 稳定会话绑定（`accounts/.agent_threads.state`）

- **绑定键（Identity）**：由 `client_session_id + account_id + space_id` 唯一决定（子 Agent 请求结合 `X-Claude-Code-Agent-Id` 生成独立子绑定键）。客户端 `session_id` 从 `metadata.user_id` JSON、显式 `session_id` 或 `conversation_id` 提取，落盘前做 SHA-256 哈希，不存储任何消息明文或凭证。
- **一会话一线程不变量**：只要绑定存在，任何非续接请求（首条消息不匹配且不属于已验证的压缩续接或未完成 `title` 辅助接管）一律返回显式 `400 [bridge:diverged]`，绝不静默分叉出第二个 Notion 线程。
- **动态上下文同步（`UPDATED CONTEXT`）**：系统提示词或工具列表变更不会新建线程，仅在下一轮跟进消息中附带一次 `UPDATED CONTEXT`。Claude Code 每轮注入的易变 `<total_tokens>N tokens left</total_tokens>` 行会被自动剔除，避免每轮重复重发系统上下文。
- **转录游标分页与浮点序号**：`getThreadTranscript` 采用 `direction: "backward", limit: 100` 沿 `backward_cursor` 逆序分页拉取（上限 32 页 / 20,000 个 patch），直至覆盖上一轮 `baseline`。转录实体序号（含 `26.5` 等浮点状态实体）统一按 `float64` 解析与排序。
- **SSE 保活与等待语义**：远端回合执行期间，流式 `/v1/messages` 在 `5s` 后发送 `message_start` 并每 `10s` 发送 `ping` 保活。若在读到完整回复前超时、断连或遇上游 5xx，返回 `[bridge:pending]`（流开始前为 HTTP 504 + `x-should-retry: true`，流开始后为 SSE `error` 事件）；相同请求重试时直接探测并重放原线程结果，绝不重复提交。

---

## 4. 本地子 Agent（`Agent` + `SendMessage`）与启动器配置

- **双轨子 Agent 路由**：
  - 默认「子 Agent / 本地子 Agent / subagent」一律调用 Claude Code 本地 `Agent` 工具（具备本地文件读写与 `Bash` 权限），并在 Notion 侧创建带 `MMDD | 子#xxxx | ...` 标题的独立线程。
  - 异步续聊通过 `SendMessage(to=<name|agentId>)` 携带相同的 `X-Claude-Code-Agent-Id` 恢复原 Notion 子线程。
  - 仅在用户显式指明「云端子 Agent / Notion 子 Agent / cloud subagent」时才调用 Notion 服务端原生 `Notion 代理`。
- **完整保留本地 Claude Code 生态（#86）**：`claude-notion.py` 使用 `--setting-sources user,project,local` 保留用户的 `~/.claude/CLAUDE.md`、Hooks、OutputStyle、Plugins、Skills 与本地 MCP 服务（通过 `--tools` 中的 `ToolSearch` 按需延迟加载 schema），同时通过高优先级 `--settings` 锁定网关地址、模型列表与底部状态栏 `scripts/notion-statusline.py`。

---

## 5. 权限映射（Approval Mode）与多账号池回退

- **权限模式（`policies.approval_mode`）**：支持 `yolo`（默认，映射 Claude Code `--permission-mode bypassPermissions`）、`ask`（映射 `default`）与 `review_for_me`（映射 `default` 并提示降级）。可通过 `claude-notion.py --notion-approval MODE`、环境变量 `AGENT_APPROVAL_MODE` 或 `config.yaml` 的 `proxy.agent_approval_mode` 配置，运行 `claude-notion.py --print-permissions` 可查看干跑 JSON 报告。
- **主账号 / 教育号（`primary` / `education`）故障转移**：账号池优先调度 `primary` 账号，仅当所有 `primary` 账号经实时配额接口（`getAIUsageEligibility`）确认为额度耗尽时才自动切换至 `education` 账号（以完整客户端历史新建线程）。
- **图片上传**：Anthropic 消息中的 PNG/JPEG/GIF/WebP 图片通过 `createAgentServiceFileUploadURL` + 签名 PUT + `completeAgentServiceFileUpload` 上传（首轮 `target.type="user"`，跟进轮 `target.type="thread"`）。
