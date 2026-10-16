# Notion Personal Agent 传输协议总览（AGENT_THREAD）

本文档是 `notion-code-bridge` Personal Agent 传输协议（`proxy.agent_thread: true`）的架构总览与专题文档索引。各模块的实现细则、协议不变量与排障手册已按主题拆分至 `docs/` 目录。

---

## 1. 核心架构摘要

- **有状态线程传输**：使用 Notion Personal Agent 的 `createAgentThread`（首轮建线程）、`sendEventToAgentThread`（同线程增量跟进）与 `getThreadTranscript`（逆序游标分页拉取），替代旧版无状态的 `runInferenceTranscript` 链路。
- **一会话一线程绑定（`accounts/.agent_threads.state`）**：以 `client_session_id + account_id + space_id`（本地子 Agent 叠加 `X-Claude-Code-Agent-Id`）作为唯一绑定键。同一会话跨轮次仅发送新增消息（`ns[rec.ConvLen:]`）；任何前缀不匹配且非合法压缩续接的请求一律显式返回 `400 [bridge:diverged]`，绝不静默分叉新线程。
- **模型矩阵与窗口对齐**：支持 `sonnet-5.5`（默认，200k 窗口）、`opus-5.5`（200k 窗口）、`gpt-6.1-sol` / `gpt-6.1-sol[1m]`（1M 窗口）与 `auto`。`claude-notion.py` 默认注入 `CLAUDE_CODE_AUTO_COMPACT_WINDOW=200000`（Sol 为 `1000000`）并为 Sonnet/Opus 5.5 写入按模型生效的 `modelSettings.<model>.autoCompactWindow=200000`（#130），使 Claude Code 终端与 Claude Desktop 3P 模式的客户端有效窗口（`180k`）与自动压缩阈值（`167k`）对齐 Notion 服务端 200k 模型窗口。
- **四层上下文压缩与四阶段载荷卸载**：客户端 `/compact` 与 Auto-Compact 摘要请求在一次性旁路线程（`beginSidecarLocked`）执行，续接回合经 SHA-256 摘要哈希校验后在原绑定线程原地 re-base；超出 170 KB（建线程）/ 120 KB（跟进）预算的提示词由 `fitAgentPromptOffload` 按 Stage 1–4 阶梯裁剪并自动挂载为 `bridge-output-N-*.txt` 附件。
- **严格工具 JSON 守卫与多层修复**：`guardToolText` 对照声明的工具 schema 校验工具调用，内置控制字符转义、正则单反斜杠修复、JSON+XML 混合闭合标签修复、顶层平铺参数提取与 `> ` 冒号修复；损坏工具调用触发 `turn.failed(maxSeq)` 回滚至 `rec.Prev` 并返回 `502 [bridge:malformed_tool_call]`。

---

## 2. 专题文档索引（`docs/`）

| 专题文档 | 核心内容 |
| --- | --- |
| **[docs/pitfalls.md](docs/pitfalls.md)** | **踩坑与排障全集（21 项）**：涵盖 20k/200k 自动压缩窗口、Claude Desktop 3P 模式 200k/1M 窗口对齐、历史大体积工具调用卸载、`/compact` 哈希对称性、本地/云端子 Agent 双轨路由、工具 JSON 修复、状态栏与 MCP 延迟加载等全部实战踩坑 |
| **[docs/agent-thread-protocol.md](docs/agent-thread-protocol.md)** | **传输协议与会话绑定**：`config.yaml` 配置、模型矩阵与 `/model` 菜单、`.agent_threads.state` 状态机、本地子 Agent（`Agent` + `SendMessage`）、权限映射（`yolo`/`ask`）、主号/教育号故障转移与图片上传 |
| **[docs/agent-thread-compaction.md](docs/agent-thread-compaction.md)** | **四层上下文压缩与载荷卸载**：手动 `/compact`（含 `<focus>` 与工具后压缩）、Auto-Compact（含工具循环中途保留尾）、Notion 服务端 ~100k 渐进式压缩检测、Stage 1–4 尺寸守卫与 `.txt` 附件卸载 |
| **[docs/agent-thread-recovery.md](docs/agent-thread-recovery.md)** | **工具守卫、中断编辑与异常恢复**：`guardToolText` 分类与 5 类自动修复、`Ctrl+C` 撤回改字重发、`claude --resume` 合成消息剥离、5xx/超时未确认回合恢复、上游 402/跨单元错误分类与脱敏诊断 |
| **[docs/cloud-agent-api.md](docs/cloud-agent-api.md)** | **个人云端 Agent 派单接口（`/v1/cloud-agent`）**：直接向 Notion 云端个人 Agent 派发异步任务、同线程续聊、查询进度与 Python 调用示例 |

---

## 3. 快速验证命令

```bash
# 1. 运行 Agent Thread 协议与压缩/守卫回归单测
go test ./internal/proxy -run TestAgent -count=1

# 2. 运行全量 Go 与启动器 Python 单元测试
go test ./...
python3 -m unittest discover -v

# 3. 编译二进制
go build -o notion-manager ./cmd/notion-manager
```
