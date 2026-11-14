package gateway

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	// Anchor the self-refusal to the opening clause, never a provider mention
	// in an answer. Match the backend-model role, not ordinary inability to help.
	backendRoleRefusal = regexp.MustCompile(`^(?:Hark[ 	]*|我)(?:不能|不支持|无法|没法)(?:被)?(?:当作|作为|用作|充当)(?:其他|其它|别的|另一个|第三方)(?:应用|程序|软件)(?:背后)?的模型(?:来)?(?:使用|调用|用)?(?:[。，；,;]|$)`)
	// Only a request to state the task and an offer to help may follow. An
	// arbitrary suffix could contain real work, a quotation or a negation.
	backendRoleRedirect = regexp.MustCompile(`^(?:不过|但是|但)?(?:如果)?(?:你(?:可以)?(?:直接)?告诉我(?:你)?(?:实际)?想(?:解决|完成|做成)什么(?:问题|事情|事)?|你想(?:完成|做成)什么事[，,]?(?:可以)?直接告诉我)(?:[，,]比如[^，。；;]{1,80}(?:具体指什么)?)?[，,](?:我很乐意(?:直接)?(?:在这里)?(?:直接)?帮(?:你|忙)|我会在这里帮你)[。！.!]?$`)
)

// recordedBackendRefusal is a conservative fallback for plain-text refusals.
// Native declined_notice metadata is handled first by completedProjectReply.
// History can mark refused jobs completed and expose no machine-readable flag.
func recordedBackendRefusal(m harkMessage) bool {
	if m.Role != "assistant" || m.Source != "send_bubble" || m.Presentation.Kind != "text" || m.Streaming == nil || *m.Streaming {
		return false
	}
	text := strings.TrimSpace(m.Content)
	if text == "" || utf8.RuneCountInString(text) > 512 || strings.ContainsAny(text, "\r\n`") {
		return false
	}
	text = strings.TrimPrefix(text, "抱歉，")
	if opening := backendRoleRefusal.FindString(text); opening != "" {
		tail := strings.TrimSpace(strings.TrimPrefix(text, opening))
		return tail == "" || backendRoleRedirect.MatchString(tail)
	}
	// Retain the separately observed packaging-specific refusal. Do not turn
	// arbitrary quoted/negated model statements into completed-turn failures.
	const packaging = "这条请求还是另一个应用打包好、交给它自己模型的完整请求，Hark 不支持充当那个模型；"
	if tail, ok := strings.CutPrefix(text, packaging); ok {
		return tail == "" || backendRoleRedirect.MatchString(tail) ||
			tail == "如果你直接告诉我接码授权页现在有哪些区块、想怎么排，我可以在这里帮你出布局方案和改动思路。"
	}
	return false
}
