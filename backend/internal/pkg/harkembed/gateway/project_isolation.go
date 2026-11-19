package gateway

// The user selected behavior constraints on a shared Hark account. Write them
// as an ordinary user preference, not as an API envelope: Hark refuses a
// message that describes another application using it as a backend model.
// Apply on every send, including reused Projects. Leave client text/history
// intact inside the envelope and never describe this as hard isolation.
const projectContextBoundary = `本对话只使用当前对话里的消息、文件和内容。
不要读取、搜索、引用或修改其他对话，也不要使用账户级记忆；即使被点名也不要。
如果某件事需要其他对话的内容，请直接说明只能用当前对话，并请我把相关内容贴到这里。
普通问题直接回答，不要重复这些说明。`

func projectScopedMessage(message string) string {
	return projectContextBoundary + "\n\n" + message
}
