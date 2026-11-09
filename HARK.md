# sub2api-hark：Hark 平台

本仓库是 [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) 的私有分支，新增 **Hark** 平台：
Hark（hark.com）网页账号像其它 API Key 账号一样在 sub2api 里导入、分组、调度、限流和计费。

## 原理

- Hark 账号 = 平台 `hark`、类型 `API Key`。
  - API Key：Hark 网页登录 Cookie，或 `{"cookie":"...","user_agent":"..."}`（登录时的浏览器 UA，Cloudflare 的 cf_clearance 和它绑定）。
  - Base URL：`http://hark.internal/v1`（默认，进程内地址，不出网）。
- 发往 `hark.internal` 的请求在 `HTTPUpstream.Do` 里交给 `internal/pkg/harkembed`，由内嵌的
  [hark2api](https://github.com/GOXF/hark2api) 网关（嵌入模式）处理：建 Project、发消息、取回复，
  用账号绑定的 sub2api 代理访问 hark.com。
- 选号、并发、冷却、换号都由 sub2api 负责；hark2api 只用指定的那个账号，Cookie 失效返回 401，被 Cloudflare 拦返回 403。
- 模型名：`hark`。原生支持三种协议，按入站协议走 hark2api 同协议端点，不做协议转换：
  - Chat Completions：`/v1/chat/completions` → `http://hark.internal/v1/chat/completions`
  - Responses（Codex，含 tools）：`/v1/responses` → `http://hark.internal/v1/responses`
  - Anthropic Messages（Claude Code）：`/v1/messages` → `http://hark.internal/v1/messages`
- `api_protocol` 未设置时 Hark 账号默认 `adaptive`（即上面的按入站协议分流）；显式设为
  `chat_completions` / `responses` / `anthropic` 时按该协议转发（其余入站协议经转换）。
  分协议地址可用 `api_base_urls` 覆盖，默认 Chat/Responses 为 `http://hark.internal/v1`，Anthropic 为 `http://hark.internal`。
- 会话头：Codex 的 `session_id` / `session-id`、Claude Code 的 `X-Claude-Code-Session-Id` 透传给 hark2api，同一对话复用同一个 Project。

## 导入账号

在「账号 → 导入」上传 `sub2api-data` 格式 JSON（HarkHubGo 2.0.23+ 或 hark2api 的账号管理页点「导出到 sub2api」即得，Cookie、UA、代理都会带上；注意文件等同密码），或手动新建：
平台选 Hark，API Key 粘贴 Cookie。

```json
{
  "type": "sub2api-data",
  "version": 1,
  "exported_at": "2026-11-04T00:00:00Z",
  "proxies": [],
  "accounts": [
    {
      "name": "hark-账号名",
      "platform": "hark",
      "type": "apikey",
      "credentials": {
        "api_key": "{\"cookie\":\"...\",\"user_agent\":\"...\"}",
        "base_url": "http://hark.internal/v1"
      },
      "concurrency": 1,
      "priority": 50
    }
  ]
}
```

## 数据

hark2api 的会话与 Project 状态存在 `$HARK_DATA_DIR`，默认 `$DATA_DIR/hark`（Docker 中为 `/app/data/hark`）。Cookie 只存在 sub2api 数据库里。

## 模型列表与任务恢复

- `/v1/models` 返回可供客户端选择的公开模型标识。账号映射里的 `*`、`claude-*` 是匹配规则，不是模型名称；Hark 的通配规则按现有 Claude 模型目录展开，精确配置的别名仍保留。
- 模型提问先分流：普通身份问题说明当前会话公开配置名，“还有哪些问法”只返回模型配置的提问示例；明确追问底层、供应商、开发者或映射时保留上游事实回答，不把不同问题都答成配置名。只按最近用户提问识别上下文追问，不以助手自述推断身份。
- 模型身份先通过系统提示告知上游当前会话选择的公开标识，实际调用上游完成生成后再归一化身份回复，不依赖回复是否包含 Hark；已正确说明配置名的自然表达保留，已识别的多余底层说明去除。不在调用上游前短路答复，不全局替换品牌或任务文本。混合问题只适配有独立段落边界的已识别身份段落，其余原样保留；边界不明确时不修改。该名称不证明底层供应商或架构。
- 身份响应适配支持三协议 JSON/SSE；为避免已经输出半截旧身份话术，只有命中身份提问的响应暂存最多 256 KiB。普通问答仍实时透传；错误、拒绝、截断、工具调用、结构化输出、引用和超限响应保持原样。真实上游用量/费用先计算，适配不伪造成功或用量。关闭模型标识开关或使用管理员自定义提示时不适配。
- 恢复会话与重新执行任务是两个动作：重载保留原 Conversation 和历史，不自动重发中断回合。继续任务应由用户明确触发，携带已保存的消息、工具调用及结果，避免重复执行已有副作用。
- 客户端 `tool_choice=required` 或指定工具名时，网关在返回前校验已声明的工具调用。缺失调用返回 `tool_call_missing`，指定工具不匹配返回 `tool_call_mismatch`（非流式 HTTP 422，流式使用协议错误事件），不自动换号重放已受理回合。必需工具回合在校验前只发送开始/保活事件，不释放未验证的助手正文；普通 `auto`/`none` 仍增量输出。实际工具由客户端执行，工具调用本身不代表执行成功。`forward-extra` 模式与必需客户端工具解析不兼容时在提交前明确报错。
- 上游返回 `cost_capped` 时保留 `upstream_budget_exhausted` 错误码，不当成普通生成失败，也不在宿主内静默换账号重放已接受的任务。生成前的 HTTP 429 仍使用原有限流处理。
- Hark API 保持浏览器 TLS 指纹；预签名附件上传使用标准 TLS，沿用同账号代理、User-Agent 与证书验证。Firefox uTLS 在存储端选择 P-256 时会发生 `invalid server key share`，此类 `upstream_upload_failed` 发生在消息提交前，不表示账号额度耗尽；换号也不能解决共同的传输故障。
- 远端 Project 按账号隔离；账号或 Project 切换后的续接以客户端保存的完整历史为依据，不依赖归档的 Project 作为唯一上下文来源。
- systemd 更新时先暂停新的网关入口并等待在途请求结束。程序的 HTTP 退出宽限期为五分钟，服务停止期限为六分钟；这不表示 Docker 或其它宿主已自动配置相同宽限期。

## 维护

- 改动面：`internal/pkg/harkembed/`、`internal/service/hark.go`、平台登记（`domain/constants.go`、`domain/platforms.go`、
  `service/domain_constants.go`、`service/provider_profile.go`）、`repository/http_upstream.go` 两行钩子、
  `openai_gateway_request_body.go` 与 `account_test_service.go` 放行 hark.internal、`account.go` 中 Hark 默认 adaptive、前端 `platformCatalog.builtin.json`。
- `.github/workflows/hark-sync.yml`：定期合并官方 main、同步 hark2api、测试通过后推送并重建镜像；合并冲突时开 issue。
  需要仓库机密 `HARK2API_TOKEN`（能读 GOXF/hark2api 的 PAT）。
- `.github/workflows/hark-image.yml`：镜像 `ghcr.io/goxf/sub2api-hark:latest`。
- 手动同步 hark2api：`tools/sync-hark2api.sh <hark2api 目录>`。同步从上游已提交的 `HEAD:gateway` 导出，再应用 `tools/harkembed-transport.patch` 保留本项目浏览器 TLS 适配；补丁冲突时停止，保留现有嵌入代码。
# 独立处理状态（opt-in）

流式请求可携带 `"hark_status": true`（或直连网关时 `X-Hark-Status: true`）。未启用时不额外订阅上游状态，也不改变既有 SSE；该字段不会发给 Hark 的消息提交接口。

网关额外发送独立的 SSE 帧，不将其拼入 assistant content、工具参数或工具回执：

```text
event: hark.status
data: {"type":"hark.status","status":"processing","conversation_id":"PROJECT","request_id":"USER_MESSAGE","message_id":"ASSISTANT_MESSAGE","line":"正在查找您的文件。","sequence":1}
```

- `processing`：来自本次关联消息的真实运行状态和公开 `narration.line`；`line_key` 若上游提供则保留。阶段文案动态透传，不读取私有 thought/reasoning。
- `waiting_for_user`：来自本次 request 的 `awaitsPerson` 或关联回复的 `inputNeeded`，清空旧阶段文案；同一请求的迟到工作事件不将其覆盖。
- `ended`：本次流已结束，不表示业务任务成功。`reason=error` 保留真实失败，`reason=stream_ended` 表示流收尾；客户端本地取消可标记 `reason=cancelled`。
- `sequence` 是每个 request 内的递增状态事件序号。客户端还须绑定自己的会话/回合/模型请求，避免旧事件跨请求、跨会话覆盖。状态只供展示，不作为已经执行工具的证据。

状态源优先使用 `/api/sync/conversation` 的 snapshot/patch；同步失败不触发模型重发或换号，并恢复历史消息中公开 narration 的回退。结束前只对已有状态连接给予最多 200ms 的收尾窗口；没有真实阶段时保持缺失。非流式响应保持原格式。

客户端应在独立区域显示“处理状态”，仅对状态展示文案去除品牌字样；不替换回复正文，也不把“等待补充”显示为“任务成功”。
