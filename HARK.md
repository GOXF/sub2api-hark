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
- 模型名：`hark`。支持 Chat Completions 与 Responses。

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

## 维护

- 改动面：`internal/pkg/harkembed/`、`internal/service/hark.go`、平台登记（`domain/constants.go`、`domain/platforms.go`、
  `service/domain_constants.go`、`service/provider_profile.go`）、`repository/http_upstream.go` 两行钩子、
  `openai_gateway_request_body.go` 放行 hark.internal、前端 `platformCatalog.builtin.json`。
- `.github/workflows/hark-sync.yml`：定期合并官方 main、同步 hark2api、测试通过后推送并重建镜像；合并冲突时开 issue。
  需要仓库机密 `HARK2API_TOKEN`（能读 GOXF/hark2api 的 PAT）。
- `.github/workflows/hark-image.yml`：镜像 `ghcr.io/goxf/sub2api-hark:latest`。
- 手动同步 hark2api：`tools/sync-hark2api.sh <hark2api 目录>`。
