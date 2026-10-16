# Notion Code Bridge 踩坑与排障全集（Pitfalls & Gotchas）

本文档汇总 `notion-code-bridge` 在对接 Claude Code 与 Notion Personal Agent（`agent_thread: true`）过程中的核心踩坑、根因分析与代码落点。

---

## 一、上下文窗口、自动压缩与尺寸预算踩坑

| # | 踩坑项与现象 | 根因分析 | 代码修复与规避方式 |
| --- | --- | --- | --- |
| **1** | **开会话或子 Agent 写代码时只有 20k 上下文、几乎每轮触发自动压缩（#102）** | `claude-notion.py` 早先将 `DEFAULT_AUTO_COMPACT_WINDOW` 设为 `"40000"`。Claude Code 内部 `dz(model, window)` 先从该窗口扣除 `20000` 输出预留得到仅 **20k** 有效窗口，`X1e` 再扣除 `13000` 摘要缓冲得到仅 **7k** 自动压缩阈值。开启 `--setting-sources user,project,local` 后首轮基础上下文（`CLAUDE.md` 48KB + `Tools` 15KB + `SessionStart` 73KB）已达 `31,561 tokens > 7,000`，导致主会话（`Ccr`）与共享进程环境变量的本地子 Agent（`inProcessRunner`）从第 1 轮起就越过压缩红线 | `claude-notion.py` 将 `DEFAULT_AUTO_COMPACT_WINDOW` 提升至 `"200000"`（`gpt-6.1-sol` 为 `"1000000"`），对应 **180k** 有效窗口与 **167k** 自动压缩阈值，与 Notion 服务端 200k 模型窗口对齐 |
| **2** | **历史回合大段 `Write`/`Edit` 工具调用参数撑爆旁路压缩或 rebind 的 170 KB / 120 KB 预算（#102）** | 当自动压缩阈值恢复到 167k 后，触发旁路摘要（`beginSidecarLocked`）或跨轮重绑（`rebindCandidate`）时会携带完整历史消息。若历史 `assistant` 回合曾通过 `Write`/`Edit` 写入上百 KB 代码（渲染在 `LOCAL TOOL REQUESTS` JSON 块中），而 `fitAgentPromptOffload` 原先只裁剪 `Role=="tool"` 结果与纯文本 `assistant` 回复，就会触发 `413 [bridge:too_large]` | `fitAgentPromptOffload`（`internal/proxy/agent_size.go` Stage 2）新增对历史回合（`i < len(msgs)-1 && len(m.ToolCalls) > 0`）中超大 `LOCAL TOOL REQUESTS` 块的截断与 `tool-req-*.txt` 附件卸载，最后一轮当前消息永不改动 |
| **3** | **自定义 Hook 带内统计尾巴或 `</summary>` 越界文本导致 `/compact` 续接报 `400 [bridge:diverged]`（#91）** | 自定义 `UserPromptSubmit` 钩子通过 `stdout` 注入指令要求模型在回复末尾贴统计栏，模型在旁路压缩时将其写在 `</summary>` 之后。Claude Code 客户端 `formatCompactSummary` 使用正则替换保留了 `</summary>` 后的尾巴，而代理 `normalizeSummary` 原先只截取 `<summary>` 内部算 SHA-256，导致续接哈希不匹配 | `normalizeSummary`（`internal/proxy/agent_compaction.go`）改为将 `<summary>...</summary>` 原地解包（保留越界文本，与客户端严格对称），并通过 `stripTrailingHookFooter` 自动剥离统计尾巴 |
| **4** | **交互模式首轮 rebind 接管标题辅助线程或加载大型 `CLAUDE.md` / `SessionStart` 报 `413 [bridge:too_large]`（#86）** | 交互模式首轮先发无工具 `title` 请求，随后带工具主请求命中 `rebindCandidate`（预算 120 KB）或首轮含 ~73 KB `SessionStart` `<system-reminder>` + ~48 KB `system`。Stage 3 原先只匹配剥离了 `cch` 的 `systemMessages(msgs)` 而漏掉原始 `system` 块，且无 `<system-reminder>` 卸载阶段 | `fitAgentPromptOffload`（`internal/proxy/agent_size.go`）在 Stage 3 同时匹配原始 `m.Role=="system"` 块，并新增 Stage 4 将 `user` 消息内超大的 `<system-reminder>...</system-reminder>` 块卸载为 `.txt` 附件 |
| **5** | **父 Shell 导出 `CLAUDE_CODE_MAX_CONTEXT_TOKENS=1000000` 导致 Claude 200k 模型误显 `1000k` 窗口（#79）** | Notion 服务端仅对 `gpt-6.1-sol`（`omniberry-sundae`）开放 1M 窗口，Claude 系列受 `oneMillionContextWindowEnabled` 门控限制固定为 200k。未清理父 Shell 变量时非 Sol 模型状态栏会串味显示 `1000k` | `claude-notion.py` 启动时先剔除继承的 `CLAUDE_CODE_MAX_CONTEXT_TOKENS`（仅在 Sol 模型下注入 `1000000`），`scripts/notion-statusline.py` 对非 Sol 模型固定按 `200k` 计算 |

---

## 二、本地子 Agent 与双轨路由踩坑

| # | 踩坑项与现象 | 根因分析 | 代码修复与规避方式 |
| --- | --- | --- | --- |
| **6** | **云端 `Notion 代理` 抢占本地 `Agent` 工具导致子 Agent 无法读写本机文件（#69）** | Notion 服务端自带原生子代理工具（`Notion 代理`）及 ~27k 服务端提示词。当协议未区分「子 Agent」归属时，模型优先调用 Notion 云端原生代理，本机 Claude Code 收不到 `tool_use` | 在 `tools.go`（`LOCAL ACTION PROTOCOL`）、`agent_thread.go` 与 `claude-notion.py` 建立双轨路由：默认说「子 Agent / 本地子 Agent」输出本地 JSON `{"name":"Agent","arguments":{...}}`；仅显式说「云端子 Agent / Notion 子 Agent」时才调云端 `Notion 代理` |
| **7** | **内置 `Explore` 子 Agent 默认传 `model: "haiku"` 触发 400 拒绝（#69 / #84）** | Claude Code 内置 `Explore` 子代理或模型默认传参可能携带 `haiku`（`CLAUDE_CODE_SUBAGENT_MODEL` 仅是默认值而非强制锁），而 Notion Agent 仅支持 `auto`、`sonnet-5.5`、`opus-5.5`、`gpt-6.1-sol` | `applyAgentSubagentDefaults`（`internal/proxy/agent_subagent.go`）在检测到子 Agent 请求头 `X-Claude-Code-Agent-Id` 且模型为 `haiku` / `claude-haiku-*` 时自动重写为 `sonnet-5.5` + `effort=high` |
| **8** | **子 Agent 运行满 30 秒被后台 `agent_summary` 摘要分叉请求抢锁（#61）** | Claude Code 每 30 秒后台发起 `querySource: "agent_summary"` 分叉请求，复用与子 Agent 完全相同的 `x-claude-code-agent-id` 和 `session_id`（仅 `tools=0` 且尾消息为摘要提示词），抢占子 Agent 线程锁或篡改绑定 | `isAgentProgressSummary`（`internal/proxy/agent_recap.go`）在 `beginKeyed` 入口拿锁前直接识别并拦截该后台摘要分叉请求 |
| **9** | **`SendMessage(to=...)` 传 `subagent_type` 报 `No agent named '...' is reachable`（#61）** | Claude Code 客户端解析 `SendMessage.to` 时仅匹配派生时显式传入的 `name` 或返回的 `agentId`，不匹配 `subagent_type`（如 `"Explore"`） | 派生需要后续 `SendMessage` 续聊的子 Agent 时必须显式传入 `name`（如 `Agent(subagent_type="coder", name="impl-worker")`），并用该 `name` 或 `agentId` 调用 `SendMessage` |
| **10** | **单回合包装头尾膨胀突破 1000B 冲击 120 KB 预算与尺寸守卫单测（#69 / #28）** | `agentThreadPromptHead` / `agentThreadFollowUpHead` / `agentThreadPromptTail` 每轮都会包裹在消息外层；头尾叠加超过 1000B 不仅挤占 120 KB 预算，还会使 `300000` 字节输入越过 `300xxx` 边界 | 详细工具协议固定放在 `LOCAL ACTION PROTOCOL` 系统消息中（首轮发送，变更时通过 `UPDATED CONTEXT` 同步）；每轮必带的头尾包装合计控制在 800 字节以内 |

---

## 三、工具调用 JSON 解析与守卫踩坑

| # | 踩坑项与现象 | 根因分析 | 代码修复与规避方式 |
| --- | --- | --- | --- |
| **11** | **`Grep` 正则调工具报 `502 [bridge:malformed_tool_call]`（#61）** | Notion 模型在 JSON 字符串字面量内直接输出非法单反斜杠转义（如 `\s`、`\d`、`\|`、`\.`），违反严格 JSON 语法 | `escapeInvalidJSONStringBackslashes`（`internal/proxy/tool_guard_escape.go`）在解析工具 JSON 前自动将字符串字面量内的非法单反斜杠补为 `\\` |
| **12** | **长多轮子 Agent 混合输出 JSON + XML 闭合标签（#61）** | Notion 模型前半段输出 `{"name":"Grep","arguments":{...}`，结尾却吐出 Anthropic 原生 XML 标签 `</parameter>\n</invoke>` 而漏写闭合 `}}` | `repairTrailingXMLToolClose`（`internal/proxy/tool_guard_escape.go`）在 JSON 值完整时自动剥离尾部 XML 标签并补齐闭合大括号 |
| **13** | **工具参数平铺顶层或键后 `:` 误写为 `>` 报 `502 [bridge:malformed_tool_call]`（#85）** | ① 模型将 `command`、`description` 直接平铺在 JSON 顶层而漏包 `"arguments":{...}`（或包在 `"input":{...}`）；② 模型把 JSON 对象键后的 `:` 误输出为 `>`（`{"command">"..."}`） | ① `parseOneCall`（`internal/proxy/tool_guard.go`）对已声明工具自动回退读取 `input` 或提取除元数据键外的顶层字段；② `repairJSONKeyAngleColon` 自动将键闭引号后的 `>` 修复为 `:` |
| **14** | **单次 `malformed_tool_call` 后重试死循环或 `SendMessage` 报 `400 [bridge:diverged]`（#61）** | 若在 `guardToolText` 校验前先调 `turn.complete(maxSeq)`，会把坏回合持久化为 `completed`，导致重试直接重放坏缓存且 `rec.MsgHashes` 停留在半截历史 | `callAgentThreadResumableTools`（`internal/proxy/agent_thread.go`）先跑 `guardToolText`，遇 `toolTextMalformed` 调 `turn.failed(maxSeq)` 回滚至 `rec.Prev` 并支持失败首轮原地续接 |

---

## 四、会话绑定、中断恢复与协议隔离踩坑

| # | 踩坑项与现象 | 根因分析 | 代码修复与规避方式 |
| --- | --- | --- | --- |
| **15** | **`submitting` 阶段 `Ctrl+C` 撤回改字死循环 + `claude --resume` 合成续接消息污染（#59）** | ① `sendEventToAgentThread` 尚未返回就被 `Ctrl+C` 取消时状态留在 `submitting`，`turn.correction` 死等远端 `idle` 线程出现 `done`；② `claude --resume` 恢复以未回答 `user` 结尾的会话时自动插入 `[assistant: "No response requested.", user: "Continue from where you left off."]` | ① `turn.correction` 探测到 `submitting` 线程处于 `idle` 且无新实体时在 `agentUnconfirmedGrace` 后立即放行重发；② `stripSyntheticResumeMessages` 在校验和渲染前剥离合成续接对并保留原始消息哈希链 |
| **16** | **会话启动偶发卡死 `409 [bridge:pending]`（#61）** | Claude Code 启动时并发发起无工具 `title` 辅助请求并中途 abort，留下未落地的 `state=submitting` 绑定阻塞同 `session_id` 主请求 | `unacceptedHelperSuperseded`（`internal/proxy/agent_binding.go`）允许带工具的主会话请求直接接管未完成且未持锁的 1 消息无工具残留绑定 |
| **17** | **`AgentThread` 模式下误触发 Legacy 原生搜索过滤导致 `WebFetch`/`WebSearch` 被剥离（#84-8）** | `HandleAnthropicMessages` 与 `finishPreparedCalls` 原先无条件执行 Legacy 模式的 `filterNativeSearchTools` / `stripWebSearchHistory` 与 `WebSearch` 拦截，篡改了 `AgentThread` 模式的客户端工具列表与历史消息哈希 | `internal/proxy/anthropic.go` 仅在 `!(AppConfig != nil && AppConfig.Proxy.AgentThread)` 时执行 Legacy 搜索过滤与拦截；`AgentThread` 模式完整保留客户端工具声明与历史 |
| **18** | **非配额错误响应已写出后误调 `RecordAccountUse` 与 `TurnCount++`（#84-3）** | `streamAnthropicTextResponse`、`handleAnthropicStream` 与 `handleAnthropicNonStream` 在写出 502/504 错误响应后返回 `nil`，导致外层 `HandleAnthropicMessages` 误判请求成功，刷新账号最后成功使用时间并递增 `Session.TurnCount` | 引入 `anthropicResponseWrittenError`（`internal/proxy/anthropic.go`），写出非配额错误后返回该包装错误，由 `HandleAnthropicMessages` 记录 `AccountErrRequestFailed` 并直接返回 |

---

## 五、启动器、MCP 延迟加载与状态栏踩坑

| # | 踩坑项与现象 | 根因分析 | 代码修复与规避方式 |
| --- | --- | --- | --- |
| **19** | **恢复 `--setting-sources user,project,local` 后本地 `settings.json` 劫持模型且 116 个 MCP 工具撑爆 170 KB（#86）** | `~/.claude/settings.json` 的 `env` / `availableModels` 覆盖了进程环境变量；`--tools` 漏写 `ToolSearch` 会禁用 MCP 延迟加载；`convertAnthropicMessages` 原先只提取 `type=="text"` 而忽略 `type=="tool_reference"` | `claude-notion.py` 通过高优先级 `--settings` 覆盖网关 `env` 与 `availableModels` 并将 `ToolSearch` 加入默认 `--tools`；代理在 `convertAnthropicMessages` 解析 `tool_reference.tool_name` 并跳过 `DeferredToolPlaceholder` |
| **20** | **`--settings` 带 `"disableAllHooks": true` 时状态栏静默不显示 + 代理模式下 `stdin.rate_limits` 恒为空（#76 / #79）** | ① Claude Code 内部 `tengu_status_line_mount` 将 `statusLine` 视同受 `disableAllHooks` 管辖的 Hook；② `extractQuotaStatusFromHeaders` 仅在官方第一方 OAuth 下填充 `stdin.rate_limits` | ① `subagent_isolation_args` 将 `"disableAllHooks": true` 替换为 `"hooks": {}`；② `scripts/notion-statusline.py` 通过 `ProxyHandler({})` 直读本机 `$ANTHROPIC_BASE_URL/health` 渲染 `6h` 与 `Cycle` 额度 |
| **21** | **Claude Desktop（`--3p` 模式）选 `Claude Sonnet 5.5 (Notion)` / `Claude Opus 5.5 (Notion)` 时底栏上下文仍显示 `X / 1M`（#130）** | ① Notion 服务端 `achira-donut`（Sonnet 5.5）与 `albuquerque-quinn`（Opus 5.5）真实窗口为 **200k**（约 50% 即 ~100k token 触发服务端 `context_compaction`），仅 `omniberry-sundae`（`GPT-6.1 Sol`）为 **1M**；② `Claude.app` 底栏分母取自内置 `claude` CLI（`2.1.299`）的 `get_context_usage.rawMaxTokens`，而 `2.1.299` 内置模型表中 `claude-sonnet-5-5` 与 `claude-opus-5-5` 带有 `native_1m: true`，即使去掉 `[1m]` 后缀或 `configLibrary` 设 `supports1m: false` 也会默认返回 `1000000`；③ `Claude.app` 拉起本地子进程时传 `--setting-sources=user,project,local`（只读 `~/.claude/settings.json`），不读 `Claude-3p/config.json` 的 `inferenceConfig.env` 与 `Claude-3p/.claude/settings.json` | ① `claude-notion.py` 在 `configLibrary` 设为 `modelPrefer1mContext: false`，仅对 `claude-sol-6-1` 启用 `supports1m: true, prefer1m: true`，`/v1/models` 令非 Sol 模型返回 `max_input_tokens: 200000`；② 在 `--settings`、`Claude-3p/.claude/settings.json` 与 `~/.claude/settings.json` 中通过 `_merge_model_settings` 为 `claude-sonnet-5-5`、`claude-opus-5-5`、`sonnet-5.5`、`opus-5.5` 写入按模型生效的 `modelSettings.<model>.autoCompactWindow = 200000`（保留原有 `effortLevel`），使桌面端与终端对 Sonnet/Opus 5.5 均准确返回 `rawMaxTokens=200000`（压缩阈值 `167k`），同时保持 `claude-sol-6-1[1m]` 为 `1000000` |

---

## 六、本仓开发与运维实操注意

1. **Go 编译入口路径**：主程序入口位于 `./cmd/notion-manager`（构建命令：`go build -o notion-manager ./cmd/notion-manager`），根目录无 `main.go`，也不存在 `./cmd/server`。
2. **本机常驻服务重启与验证**：本机服务监听 `127.0.0.1:18081`，日志文件为 `/path/to/notion-agent-bridge/runtime.log`，会话绑定状态保存在 `accounts/.agent_threads.state`。修改代理代码后需重编二进制并重启 `18081` 进程才会生效。
