# Codex 本机附件中转（Windows）

只改变文件在请求中的传输方式，不改桌面安装包、模型名、原 API Key 或上游配置。Codex 送来的附件卡片如果只有本机路径，中转在 Windows 弹窗确认后，将原始字节放进**当前会话这轮**的 `input_file`，发送给原模型线路。不会打开其他文件、运行模型命令，或在服务器读取 Windows 路径。

## 测试

1. **启动本机中转。** 在 `backend` 目录构建 `go build -o codex-attachments.exe ./cmd/codex-attachments`，或使用本地构建好的 exe。在 PowerShell 运行：

   ```powershell
   $dir = Join-Path $env:TEMP 'Codex-附件测试'
   New-Item -ItemType Directory -Path $dir -Force | Out-Null
   Set-Content -LiteralPath (Join-Path $dir 'test.txt') -Value 'CODEX-ATTACHMENT-TEST' -Encoding ascii
   $env:CODEX_ATTACHMENT_TOKEN = python -c 'import secrets; print(secrets.token_hex(32))'
   Write-Host "本机令牌（只填到本机设置，不要发到聊天）：$env:CODEX_ATTACHMENT_TOKEN"
   .\codex-attachments.exe --dir "$dir" --upstream 'http://localhost:64719/v1'
   ```

   `--upstream` 填你原来的模型 Base URL，以上是本机接入示例。中转只绑定 `127.0.0.1:64720`；保留该窗口运行。令牌不是上游 API Key，不会发给上游。

2. **在 Codex 试用这个地址。** 先备份原本的配置；只对试用的 Provider 修改 `base_url`，该 Provider 的其余配置保持原样。示例以 `codex_local_access` 为 Provider：

   ```toml
   # 合入已有表，别重复定义同一个表或键。
   [model_providers.codex_local_access]
   base_url = "http://127.0.0.1:64720/v1"
   wire_api = "responses"
   supports_websockets = false

   [model_providers.codex_local_access.http_headers]
   "X-Codex-Attachment-Token" = "粘贴刚才生成的本机令牌"
   ```

   重启 Codex 读取试用配置。不改原来的 `model_catalog_url`。这些设置不会自动写入本机配置。

3. **在新会话选文件。** 拖入测试目录里的 `test.txt`（也可以用「文件和文件夹」选它），问「请原样输出附件第一行」。核对确认框的具体文件和目的地，再点「是」。成功时应回 `CODEX-ATTACHMENT-TEST`；在同一会话再问一次，不需要重复确认这份历史附件。

## 关闭与限制

- 恢复原 Provider 的 Base URL 和其他被修改的字段，删除新增的本机令牌头，重启 Codex；再在中转窗口按 `Ctrl+C`。线上、账号和分组不需要改。
- 只读取指定本机目录中**这一轮选中的普通文件**；拒绝目录、链接、网络路径。只保留已核准的内容快照在本进程内存中供同一会话续问，授权缓存最久有效 30 分钟。中转重启、授权过期或历史重排后，用新会话重新选文件。
- 已携带内容的附件不重复读取。不按扩展名限制格式，是否能解析由上传方决定。每个文件最多 20 MiB、一轮最多 16 个文件，并受请求总大小限制。此中转不处理其他协议，不透明代替上游管理 API。
