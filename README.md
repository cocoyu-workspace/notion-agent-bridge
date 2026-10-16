# Notion Agent Bridge

将自己的 Notion AI 账号连接到本机 Claude Code 和兼容 Anthropic / OpenAI 协议的编程客户端。

这是用于个人本机部署的公开版本。使用者自行注册 Notion 账号、开通所需 AI 权限并提供自己的登录凭证。本项目不提供账号、共享 Token、订阅额度或托管账号池。后续账号池相关开发在独立私有仓库进行，公开版本不自动同步私有仓库。

## 准备

- Go 1.26 或更高版本。
- Python 3、PyYAML，以及已安装的 Claude Code（如需使用启动器）。
- 你自己的 Notion 账号和可用的 Notion AI / Personal Agent 权限。模型和额度以当前账号实际权限为准。

## 本机部署

```bash
git clone https://github.com/cocoyu-workspace/notion-agent-bridge.git
cd notion-agent-bridge
cp example.config.yaml config.yaml
go build -o notion-manager ./cmd/notion-manager
```

先按 [凭证配置指南](docs/credentials.md) 获取你自己的 Notion 登录凭证，再启动：

```bash
# 在本机终端交互输入，避免把 Token 字面量写进命令历史。
read -rs NOTION_TOKEN_V2
export NOTION_TOKEN_V2
./notion-manager
```

也可先运行 `./notion-manager`，访问本机 Dashboard `http://127.0.0.1:8081/dashboard/`，用“添加账号”导入自己的登录态。不要导入不属于自己的账号。

首次启动会生成随机的本地 `server.api_key` 并写入 `config.yaml`。客户端使用这个本地密钥，服务端使用你的 Notion 登录态。两者不同，均不得公开。

Dashboard 和管理接口没有独立登录保护，公开版默认只监听 `127.0.0.1`。请保持本机监听。

## 启动 Claude Code

在另一个终端执行：

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install PyYAML
python claude-notion.py
```

要在其他项目中使用，进入目标目录后用绝对路径运行本仓库的 `claude-notion.py`。启动器使用独立配置，不覆盖全局 Claude Code 配置。

## 接口与文档

- Anthropic Messages：`POST /v1/messages`。
- OpenAI Chat Completions / Responses：`POST /v1/chat/completions`、`POST /v1/responses`。
- 模型列表：`GET /v1/models`。
- 本地管理面板：`/dashboard/`。
- [凭证申请与本机配置](docs/credentials.md)。
- [个人 Agent 协议说明](AGENT_THREAD.md)。
- [个人云端 Agent 接口](docs/cloud-agent-api.md)。
- [排障记录](docs/pitfalls.md)。

公开版移除了批量注册、Microsoft 自动登录和注册任务管理。现有个人账号管理与协议兼容代码保留，后续账号池功能不从私有仓库自动发布。

## 验证

```bash
go test ./internal/proxy -run TestAgent -count=1
go test ./...
python3 -m unittest discover -v
go build -o notion-manager ./cmd/notion-manager
```

构建前端可运行 `cd web && npm ci && npm run build`，然后将 `web/dist/` 替换到 `internal/web/dist/` 并重新构建 Go 二进制。

## 来源与许可证

基于 [SleepingBag945/notion_manager](https://github.com/SleepingBag945/notion_manager) 及 Notion Code Bridge 的协议适配独立维护。保留原作者与贡献者署名，继续采用 [CC BY-NC-SA 4.0](LICENSE)，仅限非商业用途。

公开仓库从清理后的源码快照建立新历史，不包含私有仓库的 Git 历史、账号文件、运行日志、内部环境脚本或个人截图。
