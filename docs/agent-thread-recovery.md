# 工具守卫、中断编辑与异常恢复（Tool Guard & Turn Recovery）

本文档说明 `notion-code-bridge` 在 Personal Agent 模式下的工具 JSON 守卫与自动修复（`internal/proxy/tool_guard.go`、`internal/proxy/tool_guard_escape.go`）、`Ctrl+C` 中断改字重发（`internal/proxy/agent_edit.go`）、未确认回合故障恢复与上游错误分类。

---

## 1. 工具调用文本守卫与多层自动修复（`guardToolText`）

在 Personal Agent 模式下，模型以 JSON 文本 `{"name":"...","arguments":{...}}` 请求执行本地工具。代理通过 `guardToolText` 对照当前请求声明的工具 schema 进行严格分类与自动修复：

### 1.1 分类规则
- **合法工具调用（`toolTextCalls`）**：全文由一个或多个已声明工具的合法 JSON 调用组成（或前半段为正常说明文本 `Preamble`、末尾紧跟合法的工具调用 JSON），且包含所有 `required` 字段 -> 转换为标准 Anthropic `tool_use` 块（前置说明文本作为 `text` 块先行发送）。
- **损坏工具调用（`toolTextMalformed`）**：形似工具调用（含 `name` + `arguments`）但 JSON 截断、无法解码、工具未声明或缺少必填参数 -> 立即调用 `turn.failed(maxSeq)` 将绑定回滚至上一稳定快照 `rec.Prev`，并返回 `502 [bridge:malformed_tool_call]`（`x-should-retry: false`），绝不将半截 JSON 当成普通文本回答返回给客户端，也绝不把坏回合持久化为 `completed`。
- **普通文本回答（`toolTextPlain`）**：正文回答、或正文中引用了代码块/示例且末尾不是待执行工具调用 -> 作为普通 `text` 块返回。

### 1.2 自动解码与语法修复层（`tool_guard_escape.go`）
在不改动已合法 JSON 的前提下，最多叠加 3 层无损修复：
1. **`ctrl`（控制字符转义）**：`escapeJSONStringControls` 将 JSON 字符串字面量内部的裸控制字符（`< 0x20`，如 `Edit` 工具 `old_string`/`new_string` 中的裸 `\t` 或 `\n`）转义为 `\t` / `\n` / `\u00xx`。
2. **`backslash`（正则单反斜杠修复，#61）**：`escapeInvalidJSONStringBackslashes` 将字符串字面量内不构成合法 JSON 转义的单反斜杠（如 `Grep` 正则中的 `\s`、`\d`、`\|`、`\.` 或 Windows 路径）补为 `\\`。
3. **`xml_close`（JSON + XML 混合闭合修复，#61）**：`repairTrailingXMLToolClose` 识别模型前半段输出 JSON 工具对象、结尾混入 `</parameter>\n</invoke>` 的混合输出，在前置 JSON 值完整时自动剥离尾部 XML 标签并补齐闭合 `}`。
4. **`angle_colon` 与顶层参数回退（#85）**：`repairJSONKeyAngleColon` 修复键闭引号后误写为 `>` 的键值分隔符（`{"command">"..."}` -> `{"command":"..."}`）；`parseOneCall` 在缺少 `"arguments"` 时自动回退读取 `"input"` 或提取除 `name`/`type`/`id`/`tool_call_id` 外的顶层字段。
5. **`string` / `html` / `escape`**：支持解包外层 `<tool_call>` 标签、Markdown 代码块围栏、HTML 实体（`&quot;` / `&#34;`）及双重转义引号 `\"`。

---

## 2. `Ctrl+C` 中断撤回改字重发与 `--resume` 续接（`agent_edit.go`）

当用户在终端按 `Ctrl+C` 取消正在执行的请求、修改最后一条用户消息并重发时，代理在同一 Notion 线程上原地处理，不报 `diverged` 也不分叉新线程：

1. **前置条件**：上一轮因客户端断连（socket 关闭）标记了 `client_interrupted: true`，且新请求的前缀消息哈希与中断前完全一致，仅尾部用户消息发生修改。
2. **剥离 `claude --resume` 合成续接对**：当使用 `claude --resume` 恢复以未回答用户消息结尾的会话时，Claude Code 会自动插入 `[assistant: "No response requested.", user: "Continue from where you left off."]`。`stripSyntheticResumeMessages` 在校验可编辑尾部和渲染 Notion 提示词前自动剥离该合成对，同时保持客户端原始哈希链不变。
3. **按远端状态分流**：
   - 若被中断回合已在远端落地（`submitted` 或转录中已有新实体），代理先排空旧回合输出（不执行其任何本地工具请求），将 `baseline` 推进到旧回合之后，再在同一线程发送带 `CORRECTION` 说明的新消息。
   - 若在 `submitting` 阶段即被取消且探测到远端线程处于 `idle` 无新实体，则在 `agentUnconfirmedGrace` 后直接发送修改后的消息，不死等也不加 `CORRECTION` 头。

---

## 3. 未确认跟进失败恢复（5xx / 断连）与丢失回复恢复

- **未确认 `submitting` 恢复**：若上一轮跟进因 5xx 或网络超时停留在 `submitting`，相同请求重试会先探测转录并直接复用；若客户端放弃旧回合直接基于上一稳定状态（`rec.Prev`）发送新消息，代理先探测远端转录：确认无新实体且线程 `idle` 时自动回滚至 `rec.Prev` 并按正常跟进发送。
- **已接收 `submitted` 但回复丢失恢复**：若远端已接收回合（`submitted`），但因代理侧断流/重启导致回复未送达客户端，且客户端将旧问题与新问题合并重发（纯用户文本尾部），代理会等待旧远端回合结束并排空其输出，随后在同一线程发送合并后的新提示词。

---

## 4. 上游错误分类与脱敏诊断（`agent_diag.go`）

- **`[bridge:quota_exhausted]` -> HTTP 402 `billing_error`（`x-should-retry: false`）**：当所有可用账号均确认配额耗尽时返回不可重试的 402（而非 503），避免 Claude Code 无意义地退避重试数分钟。
- **`[bridge:notion_cross_cell]` -> HTTP 502（`x-should-retry: false`）**：当 Notion 返回 `HTTP 500 name=MemcachedCrossCellError` 时，提示切换 `proxy.notion_api_base`（`https://www.notion.so/api/v3` 与 `https://app.notion.com/api/v3` 互换）并重启服务。
- **零内容泄露的请求形状诊断（`describeAgentRequestShape`）**：提交失败时仅记录字节数、行数、最长行、工具块计数、控制字符/NUL/非 BMP 计数与错误 JSON 的键名列表，绝不在日志或错误消息中打印任何提示词正文、邮箱或凭证。
