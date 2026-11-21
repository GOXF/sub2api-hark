# Codex 附件路径读取闭环

此功能是**网关负责的附件摄取**，不是 Hark 原生客户端工具调用，也不是通用电脑控制桥。不修改 Codex、不安装插件、不绑定每台电脑的 MCP；执行仍受客户端已有的沙箱与审批控制。

## 链路

1. 经认证的 Hark Responses 请求，当前用户消息包含 Codex 的 `# Files mentioned by the user:` 文件清单，但没有附件字节。
2. 在模型调度和计费之前，网关返回客户端已声明的标准 `exec_command` 调用。命令由固定模板生成，只读取清单中的指定文件，向一次性端点 PUT 原始字节；不执行模型生成的 shell。
3. 客户端自动回传 `function_call_output`。长上传返回进程会话时，网关只用 `write_stdin` 空输入等待同一次上传，不要求用户发送“继续”。
4. 校验调用方、会话、原始用户历史、发出的命令、回执和实际字节哈希；将字节放入已有附件管线，走 presign → PUT → finalize → 消息附件。
5. 上传调用及回执不作为模型输入，也不额外按模型请求计费；真正发送 Hark 的续轮仍按原规则计费。用户原始消息哈希不变，后续历史不重置、不重复上传。

## 部署条件与限制

- 管理员现有的“前端地址”设置 `GetFrontendURL`（配置 fallback：`server.frontend_url`）必须指向**承载 API 的 HTTPS 源站**，例如 `https://sub.fanjj.com`，不能是另一台只托管前端的域名。只允许根源站，不接受用户名、密码、路径前缀、查询或片段；HTTP 仅允许 loopback 测试。不会使用客户端 Host 或转发头补齐地址。
- 外层路由必须转发 `PUT /v1/attachment-reads/:id`。此端点只接受不可猜测的一次性能力，不需要客户端 API Key/Cookie，不提供下载，拒绝浏览器 Origin；客户端不跟随上传重定向。
- 目前只接入已由原版 Codex 实测的 flat `exec_command`，长任务等待使用 flat `write_stdin`。只在 `tool_choice=auto` 时发起；没有工具、权限拒绝、指定其他工具等情况明确失败，不绕过审批。普通聊天与已经携带字节的附件保持原链路。
- 沿用现有每文件大小与每请求附件数量限制（默认 20 MiB、16 个），不新增文件格式白名单。上游不支持的格式仍由原上传错误处理返回，不在用户错误中暴露平台名称。
- 临时字节保留 5 分钟，总字节预算 64 MiB，上传读取前先预留；最多 64 个票据、同一 API Key 最多 16 个尚未过期票据。预算已满明确返回 429。借用中的字节仍计入预算；过期上传不保留。
- 持久化的 `attachment-call.key` 只用于识别网关自己的历史调用，不保存文件字节或上传能力。进程重启会丢失未完成上传；旧历史仍可识别，当前未完成读取明确返回过期，不暗中重发。

## 验证

常规回归（无需真实账号）：

```sh
cd backend
go test ./internal/pkg/harkembed/... ./internal/server/routes -count=1
```

显式原版客户端验收：设置 `CODEX_BINARY`、`HARK_ATTACHMENT_FIXTURE`、`HARK_CODEX_ATTACHMENT_DIR`，运行 `go test ./internal/pkg/harkembed -run '^TestAttachmentNativeCodex$' -count=1 -v`。夹具必须位于隔离测试目录，只使用合成内容。测试驱动复制现有权限字段，不修改真实配置、不设置审批/提权覆盖；当前驱动面向已验证的 Windows 保存配置。

默认上游是模拟 Hark，最终标记只能从实际上传字节提取。`HARK_ATTACHMENT_SLOW_RECEIPT=true` 用 33 秒延迟验证真实客户端自动等待。真实上游必须另行提供经授权的私有账号 helper，其 stdout 仅经内存管道交给测试；不能把模拟验收标为真实 Hark 成功。真实失败保留在外部证据目录，不重发已受理请求。
