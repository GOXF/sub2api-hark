# Hark 只读原结果恢复

管理员接口（不是模型生成或工具执行接口）：

```http
GET /api/v1/admin/accounts/ACCOUNT/hark/sessions/SESSION/result
x-api-key: ADMIN_KEY
```

- 使用管理员 JWT 或全权管理密钥。普通模型 API Key 和仅有账户读取权限的受限管理密钥不能读取会话正文。
- 仅访问该账号已保存会话映射指向的原 Project；不接受调用方指定 Project，也不调度、换号或直连回退。
- 保存了 `pending_turn` 的请求默认使用其中的原生关联 ID。旧记录必须成对提供 `request_id`、`response_message_id` 查询参数；读取后还要核对原生用户消息与主任务的关联，不能仅凭调用方声明。
- 一次请求最多进行一次上游历史 GET，30 秒超时。不会创建/发送/归档、执行工具、更新游标或 LastUsed、清除 pending、安装摘要或覆盖旧上下文。读取期间会话发生变化时返回冲突。

诊断结果示例：

```json
{
  "read_only": true,
  "continuation_applied": false,
  "status": "awaiting_client_tool",
  "result": {"text": "<tool_call>原工具请求文本</tool_call>", "attachments": null}
}
```

`status` 区分 `processing`、`awaiting_input`、`completed`、`awaiting_client_tool`、`declined`、`failed`。HTTP 200 只表示成功读取诊断，不表示原任务成功。失败还附带 `error_code`，拒绝原文不替换。包含工具标记的已完成结果仅标为待人工审阅的客户端工具请求，不返回可执行的模型 `tool_calls`。

**读取原结果不等于恢复客户端并续跑任务。** 本接口不自动实施后续修改，不改变 `session_turn_pending` 保护；后续如何安装结果和继续执行需单独设计、授权和验证。
