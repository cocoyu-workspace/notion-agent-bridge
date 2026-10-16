# 四层上下文压缩与载荷卸载机制（Compaction & Payload Offloading）

本文档说明 `notion-code-bridge` 在有状态 Notion Agent 线程上的四层上下文压缩与载荷裁剪架构（`internal/proxy/agent_compaction.go`、`internal/proxy/agent_size.go`）。

---

## 1. 四层压缩架构总览

| 层级 | 组件与代码入口 | 核心职责 |
| --- | --- | --- |
| **第 1 层：客户端窗口与触发对表** | `estimatedInputTokens`（`anthropic.go`）+ `DEFAULT_AUTO_COMPACT_WINDOW=200000`（`claude-notion.py`） | 向 Claude Code 报告字节估算的 `input_tokens`，并将默认自动压缩窗口对齐 Notion 的 200k 模型窗口（`180k` 有效窗口 / `167k` 触发阈值，`gpt-6.1-sol` 为 `1000000`） |
| **第 2 层：旁路一次性摘要线程（Sidecar）** | `beginSidecarLocked`（`agent_compaction.go`）+ `agent_summary_fingerprint.go` | 精确匹配 Claude Code 的 9 段式 `<analysis>+<summary>` 压缩指令指纹，在一次性 Notion 旁路线程（`createAgentThread`）上生成摘要并记录摘要 SHA-256（`AuxSummaryHash`），保持主绑定线程不被污染 |
| **第 3 层：SHA-256 续接校验与原地 Re-base** | `verifyClaudeCompaction` / `manualCompactKeptTail` / `autoCompactKeptTail`（`agent_compaction.go`） | 校验下一轮请求携带的 `<summary>` 哈希与 `AuxSummaryHash` 完全一致（一次性使用），剥离客户端压缩脚手架，仅将 `[CLIENT CONTEXT COMPACTION + 摘要 + 保留尾部的新增消息]` 发送至原绑定线程并原地重置 `RootHash` / `ConvLen` / `ConvHash` |
| **第 4 层：服务端渐进式压缩检测与四阶段载荷卸载** | `agentTranscriptCompacted`（`agent_thread.go`）+ `fitAgentPromptOffload`（`agent_size.go`） | 检测 Notion 服务端约 100k token 处的 `context_compaction` 实体并在下一轮补发一次协议上下文；对超出 170 KB（建线程）/ 120 KB（跟进）预算的请求按 Stage 1–4 阶梯裁剪并卸载为 `.txt` 附件 |

---

## 2. 客户端手动 `/compact` 与自动压缩（Auto-Compact）

### 2.1 支持的压缩形态
1. **标准手动 `/compact` 与 `/compact <focus>`**：
   - 识别 `claude-code-2.1.280` 与 `2.1.296` 的完整摘要指令指纹（以及插入在 `REMINDER:` 前的 `Additional Instructions:\n<focus>\n\n`，`focus` 长度 ≤ 4096 字节）。
   - 续接回合验证首条消息中的摘要前缀、`Summary:`、转录路径指针、`<command-name>/compact</command-name>` 与新增用户文本，校验 `AuxSummaryHash` 后在原线程原地 re-base。
2. **自动压缩（Auto-Compact，无保留尾与带保留尾）**：
   - 摘要请求将压缩指令作为待发用户消息的最后一个文本块，或丢弃最后一轮 `assistant` 回复后附加在上一条 `user` / `tool_result` 消息末尾（`rewoundSummaryRequest` 记录 `aux_rewound: true`）。
   - 续接请求支持无保留消息形态、保留最后一轮纯文本 `assistant` 回复形态、以及**工具循环中途（Mid-tool-loop）**保留带 `tool_use` 的 `assistant` 回复加对应 `tool_result` 形态（`autoCompactKeptTail`）。由于原 Notion 线程已持有该保留回复，代理仅向原线程转发摘要与新增的 `tool_result` / 用户消息。
3. **工具调用后的手动 `/compact`（Post-tool manual `/compact`）**：
   - `convertAnthropicMessages` 将带有 `[tool_result, instruction]` 的混合消息拆分为 `Role: "tool"` 与 `Role: "user" (AfterToolResult: true)`，续接时由 `manualCompactKeptTail` 剥离命令包装块并原地 re-base。

### 2.2 摘要哈希对称归一化（`normalizeSummary`）
- 客户端 `formatCompactSummary` 在解包 `<summary>...</summary>` 时会保留标签前后的越界文本。代理端 `normalizeSummary` 同样采用原地解包（而非仅截取标签内部），并通过 `stripTrailingHookFooter` 自动剔除自定义 Hook 可能混入的尾部统计行，确保两端计算的 SHA-256 严格一致。

---

## 3. Notion 服务端渐进式压缩（Server-Side Compaction）

- Notion 服务端对 Claude 200k 窗口采用渐进式转录压缩（`createSummaryThreshold: 0.5`、`updateSummaryInterval: 0.2`、`threshold: 0.9`），约在 **100k token** 处首次生成 `context_compaction` 工具实体（网页端显示「已压缩背景信息」）。
- 代理在每轮完成时通过 `agentTranscriptCompacted` 检查是否存在新的 `context_compaction` 实体。一旦检测到，即递增 `notion_compactions` 计数并将 `ResendContext` 置为 `true`，使下一轮跟进请求自动重发一次 `LOCAL ACTION PROTOCOL` 系统与工具上下文，防止服务端压缩把本地 JSON 工具协议摘要丢弃。

---

## 4. 四阶段尺寸守卫与 `.txt` 附件卸载（`fitAgentPromptOffload`）

Notion 上游对单次请求大小有硬限制：建线程（`createAgentThread`）安全预算为 **170,000 字节**，跟进（`sendEventToAgentThread`）安全预算为 **120,000 字节**。当单轮提示词超出预算时，`fitAgentPromptOffload`（`internal/proxy/agent_size.go`）按以下四个阶段从大到小裁剪，并将最多 **8 个**被裁剪块的完整原文通过 Agent 文件接口上传为 `bridge-output-N-<label>.txt` 附件挂在同一条消息上（内联保留头尾各至少 2048 字节及指向附件文件名的说明标记）：

1. **Stage 1（工具输出优先）**：裁剪 `Role == "tool"` 的本地工具执行结果（`tool-<call_id>`）。
2. **Stage 2（历史子任务通知与历史 Assistant 块）**：裁剪超大的 `<task-notification>` 子 Agent 结果、历史回合（`i < len(msgs)-1`）的 `assistant` 文本块（`message-K`）以及历史回合超大的 `LOCAL TOOL REQUESTS` 工具调用参数 JSON 块（`tool-req-K`，如历史大体积 `Write`/`Edit` 代码内容）。
3. **Stage 3（项目与客户端系统上下文）**：将重发或首轮超大的 `system` 上下文块卸载为 `context-K.txt` 附件（若无附件槽位则截断中间并提示沿用线程早期完整版本）。
4. **Stage 4（用户消息内的 `<system-reminder>` 块）**：将 `user` 消息中由 `CLAUDE.md`、`SessionStart` Hook 或 Skills 注入的超大 `<system-reminder>...</system-reminder>` 块卸载为 `reminder-K.txt` 附件，**用户自己输入的纯文本一字不改**。

若经过 Stage 1–4 后仅用户纯文本本身仍超过预算，则在发送前直接拒绝并返回 `413 [bridge:too_large]`（`x-should-retry: false`），不污染远端线程，绑定状态保持完好。
