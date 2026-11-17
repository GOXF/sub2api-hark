# 持久化恢复：一次投递、回执和工具输出尾部

## 后续本地候选：提交失败分类与证据保留

`codex/hark-submission-outcome-20261116` 仅在本地补充分类，不改变下面已发布协议的回执、清除或重试权限。

- 新增 `pending_turn.submission_failure`，保存 `outcome`（`not_sent` / `rejected` / `uncertain`）、提交阶段、原错误代码/语义状态、实际上游 HTTP 状态、脱敏原错误/传输错误、响应前缀 SHA256 与有界摘要。响应最多取前 64 KiB，文本摘要最多 8 KiB；凭据脱敏。哈希属于捕获前缀，不冒称无限长原响应的完整哈希。
- 原生 HTTP 402 只有完整 JSON 中 `code=token_limit_reached`、`error` 为非空字符串且无 `success/accepted/redirected=true` 冲突时才判为 `rejected`，保留 `upstream_quota_exhausted` 与原始响应证据，对外 HTTP 402。未知、代理 HTML、截断或矛盾响应仍为 `uncertain`。此分类不解锁、不换号、不重发；已保存的旧 pending 及旧分类不追溯改写。
- 仅完整读取、符合现有原生错误形状的 400/401/403/413/415/422/429，且没有矛盾的成功/受理标志，才分类为 `rejected`。HTML/代理拦截、部分响应、5xx、连接中断、无效 JSON、关联不符或受理保存失败保留为 `uncertain`；本地组包失败为 `not_sent`。不据此猜测上游接受性或绕过拒绝。
- 公开失败保持 `recovery_*` 请求级代码，同时保留原错误代码与实际 HTTP 状态的说明；401 的原网关语义仍为502，不冒充客户端 API Key401。宿主不自动换号、冷却账号或重发。重复请求和重启返回同一已保存原因，不再统一丢成无来源的409。
- 原管理员只读 result 入口可直接返回保存的 `submission_failure` 元数据，状态为 `submission_rejected` / `submission_uncertain` / `submission_not_sent`，`read_only=true`、`continuation_applied=false`；它不是成功模型结果，无工具输出，无 native 历史 GET，不刷新游标或清 pending。
- **本候选不清任何 pending，也不自动解锁明确拒绝的会话**。失败证据写入失败时，返回503并附原原因，保持旧 intent；重启不能凭未落盘证据认领成功。旧记录缺原错误证据时不补造原因；账号39的既有失败不能从这次修改追溯恢复。

状态：**仅本地候选，未推送、未部署**。仅修改 sub2api-hark 服务端；没有改客户端、重发真实旧任务或执行真实工具。本候选在源码中注册管理员控制路由，不代表运行服务器已开放这些路由。

## 身份与原始契约

- API Key 认证成功后，将数据库中的稳定 Key ID 写入可信 request context。`harkembed.Do` 用该值构造 `Server.CallerHandler("sub2api-key-ID")`；HTTP header 无法选择或覆盖 owner。
- 原账号、session、Project、native request/response IDs 与 owner 一起保存在 `project-sessions.json`。请求提交前保存原协议路径、JSON 规范化请求哈希、完整工具 schema、工具选择、历史指纹、模型及流式选项；不是保存原封包的所有原始字节。
- 旧记录没有 owner、原契约或已受理证明时，只能诊断，不能根据当前请求认领历史。跨调用方拒绝；不换账号、Project，不清除 pending。
- 凭据和真实配置仍留在原运行环境；本轮证据只使用临时合成数据。

## 状态转换与安全停止

```text
原提交 intent 落盘 -> native POST -> accepted 落盘 -> 结果读取中断
  -> 管理员 prepare：只读获取原关联结果，验收原文及工具契约
  -> ready：完整 wire、response/call IDs、原文/请求/wire 哈希落盘
  -> issued：先持久化，再输出一次协议响应
     -> 无工具：明确 installed 回执 + delivery/result 身份 -> acknowledged
     -> 有工具：原历史前缀 + 回显 call IDs/名称/参数 + 全部工具输出
        -> continuing 和新 pending intent 先落盘
        -> 只 POST 新的工具输出尾部 -> accepted -> continued
```

`issued` 后即使写出失败、客户端断线或服务重启，也不会自动再次下发工具。同一原请求或已消费尾部返回 `recovery_delivery_uncertain` / 409。这里保证的是网关不自动重复发行，不宣称客户端工具具有全局 exactly-once 语义。

持久化失败时恢复内存中的上一个状态，不输出工具、不发送新尾部。POST 受理不明时保留新 intent，以 `recovery_submission_uncertain` / 409 停止，重启不自动重试。`recovery_*` 与 `session_owner_mismatch` 都属于请求级错误，宿主不能通过换号重发修复。

普通答案的回执要求管理员明确确认已安装；HTTP 200、SSE 结束不自动确认安装。有工具时，`installed:true` 不能清 pending，必须核验工具输出回执。并发投递和回执以会话锁及原子存储推进；去重日志最多 4096 项，超限拒绝，不丢弃旧项。存储总预算 64 MiB，已保存 wire 重启时核验哈希；pending 不参与闲置归档，不能通过 end 绕过。

## 管理员控制接口

沿用宿主管理员认证；普通模型路由和 scoped account 权限不能访问控制接口。宿主始终指定原账号凭据和代理，不经过账号调度或身份改写。

```text
POST /api/v1/admin/accounts/ACCOUNT/hark/sessions/SESSION/recovery/prepare
{
  "owner_id": "sub2api-key-KEY_ID",
  "request_id": "NATIVE_REQUEST_ID",
  "response_message_id": "NATIVE_RESPONSE_ID",
  "request_sha256": "ORIGINAL_CANONICAL_REQUEST_SHA256",
  "result_sha256": "REVIEWED_ORIGINAL_RESULT_UTF8_SHA256"
}

POST /api/v1/admin/accounts/ACCOUNT/hark/sessions/SESSION/recovery/ack
{
  "owner_id": "sub2api-key-KEY_ID",
  "delivery_id": "PREPARED_DELIVERY_ID",
  "result_sha256": "REVIEWED_ORIGINAL_RESULT_UTF8_SHA256",
  "installed": true
}
```

`prepare` 只验收已完成或等待客户端工具的原结果。拒绝、处理中、预算/生成失败、附件未验收、未知工具或哈希不符均不安装；拒绝原文不被成功答案替换。`ack` 仅适用于无工具的已投递答案，重复有效回执幂等。有工具时由原调用方下一份模型请求携带完整原历史与工具结果，服务端验证后只发送新增工具输出。

## 本地验收与未覆盖范围

三协议 Chat / Responses / Messages 的 JSON/SSE 共六种形态使用合成上游验证：原任务一次 POST、恢复投递一次、新工具输出一次 POST、归档零次；重复投递、重复尾部、跨 owner、存储失败、断线及重启均覆盖。原拒绝识别和上下文保护回归仍保留。

已安装且未修改的 Codex app-server 在临时 CODEX_HOME、空目录和临时会话中，接收一次 `fixture_probe` 动态模拟调用、返回常量、继续并以 `completed` 结束。夹具在合成受理后读取失败处插入显式管理员 prepare；没有加载真实失败会话，也没有证明现有客户端会自动重试或恢复旧任务。

以下边界仍然存在：

1. 旧账号 50 的存档没有可信 owner/契约时，不能靠本候选自动恢复。
2. 已发行但没有可靠回执的工具响应仍停止等待核对，不能为了“有回复”而重复执行。
3. 未准备恢复的普通响应，其所有断线及安装确认窗口不在本轮完整覆盖范围内。
4. 历史指纹、schema 或客户端完整前缀改变时保守拒绝；增量上下文协议不在本轮支持范围。
5. 回退演练仅还原隔离源码副本。旧二进制不理解新增 ledger，生产数据的混版本写入、跨版本续接和真实服务器发布尚未验收。
