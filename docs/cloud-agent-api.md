# 个人云端 Agent 派单接口指南（`/v1/cloud-agent`）

本文档说明如何通过本机 HTTP 接口 `POST /v1/cloud-agent` 与 `GET /v1/cloud-agent` 直接向已导入账号的 Notion 个人云端 Agent 派发任务、同线程续聊与查询进度。


---

## 1. 接口协议概览

- **鉴权与监听要求**：要求 `Authorization: Bearer <local API key>` 或 `x-api-key` 请求头，且仅允许回环地址（Loopback，如 `127.0.0.1`）访问。
- **新建任务（`POST /v1/cloud-agent`）**：
  ```json
  {
    "requestId": "stable-job-0001",
    "accountId": "完整导入账号 ID（account_id）",
    "category": "调试",
    "title": "NCB #28 云端修复",
    "task": "你的原始任务指令"
  }
  ```
  `title` 与 `category` 可选；省略时代理自动将 Notion 线程标题格式化为 `MMDD | 云 | <分类> | <主题>` 并通过 `saveTransactionsFanout`（`user_set_title: true`）锁定标题。
- **同线程续聊（`POST /v1/cloud-agent`）**：
  ```json
  {
    "requestId": "stable-job-0002",
    "threadId": "首轮返回的 threadId",
    "task": "下一轮跟进指令"
  }
  ```
- **查询指定请求（`GET /v1/cloud-agent?requestId=...`）**：读取该请求固定结束序号（end sequence）内的状态、进度 patch（含原生工具执行回执）与回复文本，后续轮次不会污染已完成请求的结果。
- **查询线程最新回合（`GET /v1/cloud-agent?threadId=...`）**：读取该线程最近一轮受管回合的状态与输出。

### 默认参数与安全边界
- 默认参数：`model: sonnet-5.5`、`effort: high`、审批模式 `yolo`、启用浏览器与 Auto computer selection（省略 `computerProvider`）。
- 显式指定 `accountId` 派单，不做跨账号故障转移或跨账号迁移线程；续聊仅允许针对本接口日志（`accounts/.cloud_agent_requests.json`）创建的已完成或已确认空闲的中断线程。
- 原始 `task` 文本原样发送给 Notion 云端 Agent，不注入本地 Claude Code 提示词或本地 JSON 工具协议，云端工作区的 Notion MCP 连接与原生 Skills 保持原样可用。

---

## 2. Python 命令行调用示例（`cloud-call.py`）

将以下脚本保存为本地调用脚本（从 `config.yaml` 读取 `api_key`，不在命令行暴露密钥）：

```python
import argparse
import hashlib
import json
from pathlib import Path
from urllib.parse import urlencode
from urllib.request import Request, urlopen
import yaml

ROOT = Path("/path/to/notion-agent-bridge")
ACCOUNT_NAME = "YOUR_ACCOUNT_NAME"  # Replace with your own imported account user_name

parser = argparse.ArgumentParser()
parser.add_argument("operation", choices=["new", "continue", "get", "thread"])
parser.add_argument("identifier")
parser.add_argument("--task-file", type=Path)
parser.add_argument("--thread-id")
args = parser.parse_args()

cfg = yaml.safe_load((ROOT / "config.yaml").read_text())
server = cfg["server"]
base = f"http://127.0.0.1:{server['port']}/v1/cloud-agent"
headers = {"Authorization": "Bearer " + server["api_key"], "Content-Type": "application/json"}

def find_account_id() -> str:
    accounts_dir = Path(server.get("accounts_dir", "accounts"))
    if not accounts_dir.is_absolute():
        accounts_dir = ROOT / accounts_dir
    for path in sorted(accounts_dir.glob("*.json")):
        rec = json.loads(path.read_text(encoding="utf-8"))
        if rec.get("user_name") == ACCOUNT_NAME:
            return rec.get("account_id") or hashlib.sha256(
                f"{rec['user_id']}\0{rec['space_id']}".encode()
            ).hexdigest()
    raise SystemExit(f"未找到 user_name == {ACCOUNT_NAME!r}")

body = None
if args.operation in ("get", "thread"):
    key = "requestId" if args.operation == "get" else "threadId"
    base += "?" + urlencode({key: args.identifier})
else:
    payload = {"requestId": args.identifier, "task": args.task_file.read_text(encoding="utf-8")}
    if args.operation == "new":
        payload["accountId"] = find_account_id()
    else:
        payload["threadId"] = args.thread_id
    body = json.dumps(payload).encode("utf-8")

req = Request(base, data=body, headers=headers, method="POST" if body else "GET")
with urlopen(req, timeout=240) as resp:
    print(f"HTTP {resp.status}")
    print(resp.read().decode("utf-8", errors="replace"))
```

### 常用命令

```bash
# 1. 新建云端任务
python3 cloud-call.py new job-0001 --task-file task.txt

# 2. 在同一云端线程续聊
python3 cloud-call.py continue job-0002 --thread-id <THREAD_ID> --task-file followup.txt

# 3. 按 requestId 或 threadId 查询状态与回复
python3 cloud-call.py get job-0001
python3 cloud-call.py thread <THREAD_ID>
```

---

## 3. 状态码与返回状态说明

| HTTP 状态码 / `status` | 含义与处理方式 |
| --- | --- |
| `200 completed` | 当前回合已完成，`reply` 字段包含完整回复 |
| `202 pending` | 远端回合仍在运行（或等待网页端人工审批）；使用相同 `requestId` 调 `GET` 查询，不要换新 `requestId` 重发 |
| `400 invalid_request / model_unavailable` | 请求参数缺失、模型不受支持或引用的 `threadId` 不存在 |
| `409 conflict` | 同一 `requestId` 参数不一致、账号不匹配或目标线程上一轮尚未完成 |
| `502 failed / rejected / transcript_incomplete` | 远端回合执行失败、被上游 4xx 拒绝或转录分页不完整 |
