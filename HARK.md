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
- 模型身份通过系统提示告知上游当前会话选择的公开标识，由上游自然组织回答；网关不截获身份问题、不合成本地固定回复。回复详略和格式遵从用户要求，身份与任务混合的问题完整转发。该名称不证明 Hark 底层实际供应商或架构。客户端应选择具体模型，而不是把通配规则作为模型提交。
- 恢复会话与重新执行任务是两个动作：重载保留原 Conversation 和历史，不自动重发中断回合。继续任务应由用户明确触发，携带已保存的消息、工具调用及结果，避免重复执行已有副作用。
- 上游返回 `cost_capped` 时保留 `upstream_budget_exhausted` 错误码，不当成普通生成失败，也不在宿主内静默换账号重放已接受的任务。生成前的 HTTP 429 仍使用原有限流处理。
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
