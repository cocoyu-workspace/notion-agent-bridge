# 自行申请账号并配置凭证

## Notion 账号与权限

在 [Notion 官网](https://www.notion.so/)自行注册或登录自己的账号，通过 Notion 自己的产品界面申请或购买所需 AI 权限。账号与订阅费用由使用者承担，项目不发放 Token。

这个桥接器使用 Notion 网页 Personal Agent 会话协议，需要本人网页登录态的 `token_v2` Cookie。Notion 开发者集成 Token（例如 `secret_` / `ntn_`）不能替代网页登录态，也不能凭空开通 AI 权限。

## 获取本人的网页登录态

1. 在本机浏览器登录自己的 Notion 账号，并确认能在目标工作区正常使用 Notion AI。
2. 打开浏览器开发者工具的 Application / Storage，再打开当前 Notion 站点的 Cookies。
3. 找到 `token_v2`，仅复制到本机终端的交互输入或本机 Dashboard。“添加账号”支持直接输入 Token，服务会发现所属用户和工作区。
4. 多工作区身份需要明确选择时，可在本机设置 `NOTION_ACTIVE_USER_ID`、`NOTION_SPACE_ID` 或 `NOTION_EXPECTED_EMAIL`，防止选错身份。

仓库里的 `chrome-extension/` 是本机导出辅助工具，可作为可选方式使用。导出的 JSON 含登录凭证，仅保存在自己的电脑上，不得上传到 GitHub。

`token_v2` 是登录凭证，具有当前账号的访问能力。不要发到 issue、聊天、截图或共享配置。不要使用别人提供的登录态。

## 两种不同的密钥

| 名称 | 来源 | 用途 |
| --- | --- | --- |
| `NOTION_TOKEN_V2` | 本人 Notion 浏览器登录态 | 桥接器访问本人的 Notion 账号 |
| `server.api_key` / `API_KEY` | 本机首次启动随机生成，或本人配置 | 编程客户端访问本地桥接器 |

首次配置可复制 `example.config.yaml`。其中不含有效凭证。启动器自动读取本地 `config.yaml` 的 `server.api_key`，其他客户端自行填写这个本地密钥。

## 存储与失效

账号发现会把登录态持久化到本机 `accounts/`。`config.yaml`、`token.txt`、`accounts/`、环境文件和日志均被 Git 忽略，但仍应妥善保管。无需将任何真实配置提交到仓库。

登录态失效时，在 Notion 重新登录并更新本机凭证。若凭证泄露，应通过对应服务的账号安全设置撤销相关会话或凭证并重新配置。删除 Git 文件不能撤销已泄露的凭证。
