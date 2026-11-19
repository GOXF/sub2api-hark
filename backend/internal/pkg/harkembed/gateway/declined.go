package gateway

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	// Anchor the self-refusal to the opening clause, never a provider mention
	// in an answer. Match the backend-model role, not ordinary inability to help.
	backendRoleRefusal     = regexp.MustCompile(`^(?:Hark[ 	]*|我)(?:不能|不支持|无法|没法)(?:被)?(?:当作|作为|用作|充当)(?:其他|其它|别的|另一个|第三方)(?:应用|程序|软件)(?:背后)?的模型(?:来)?(?:使用|调用|用)?(?:[。，；,;]|$)`)
	backendRoleTaskPrompt  = regexp.MustCompile(`^(?:不过|但是|但)?(?:如果)?(?:你(?:可以)?(?:直接)?(?:在这里)?告诉我(?:你)?(?:实际)?想(?:解决|完成|做成|做)什么(?:问题|事情|事)?|你想(?:完成|做成)什么事(?:(?:可以)?直接告诉我)?)$`)
	backendRoleExample     = regexp.MustCompile(`^比如[^，。；;]{1,80}(?:具体指什么)?$`)
	backendRoleInputPrompt = regexp.MustCompile(`^(?:你(?:可以)?|可以)?(?:直接)?(?:在这里)?(?:(?:把|将)(?:图|图片|截图)(?:发给我|发过来)|(?:发|发送)(?:图|图片|截图)给我|告诉我)$`)
	backendRoleHelpOffer   = regexp.MustCompile(`^(?:我很乐意(?:直接)?(?:在这里)?(?:直接)?帮(?:你|忙)|我会在这里帮你)$`)
)

// A redirect can contain examples and requests for input, not an arbitrary
// suffix. Classify each clause so another input invitation does not turn a
// refusal into success, while real work, quotations and negations stay excluded.
func backendRoleRedirect(text string) bool {
	clauses := strings.FieldsFunc(strings.TrimRight(strings.TrimSpace(text), "。！.!"), func(r rune) bool {
		return r == '，' || r == ',' || r == '。' || r == '；' || r == ';'
	})
	if len(clauses) < 2 || !backendRoleTaskPrompt.MatchString(strings.TrimSpace(clauses[0])) ||
		!backendRoleHelpOffer.MatchString(strings.TrimSpace(clauses[len(clauses)-1])) {
		return false
	}
	for _, clause := range clauses[1 : len(clauses)-1] {
		clause = strings.TrimSpace(clause)
		if !backendRoleExample.MatchString(clause) && !backendRoleInputPrompt.MatchString(clause) {
			return false
		}
	}
	return true
}

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
	// Opening variants observed from the live model. They are anchored so a
	// quote, negation or ordinary answer that merely mentions the phrase still
	// passes through.
	for _, prefix := range []string{
		"Hark 不能充当其他应用背后的模型",
		"Hark 不能当作其他应用背后的模型",
		"Hark 不支持作为其他应用背后的模型",
		"Hark 不支持被当作其他应用背后的模型",
		"Hark 不能被用作其他应用背后的模型",
		"Hark 不能作为其他应用背后的模型",
		"我没法充当其他应用",
		"我不能这样读取。Hark 不能充当其他应用背后的模型",
	} {
		if strings.HasPrefix(text, prefix) {
			return backendRefusalTail(strings.TrimPrefix(text, prefix))
		}
	}
	if opening := backendRoleRefusal.FindString(text); opening != "" {
		tail := strings.TrimSpace(strings.TrimPrefix(text, opening))
		return tail == "" || backendRoleRedirect(tail)
	}
	// Retain the separately observed packaging-specific refusal. Do not turn
	// arbitrary quoted/negated model statements into completed-turn failures.
	const packaging = "这条请求还是另一个应用打包好、交给它自己模型的完整请求，Hark 不支持充当那个模型；"
	if tail, ok := strings.CutPrefix(text, packaging); ok {
		return tail == "" || backendRoleRedirect(tail) ||
			tail == "如果你直接告诉我接码授权页现在有哪些区块、想怎么排，我可以在这里帮你出布局方案和改动思路。"
	}
	return false
}

// backendRefusalTail separates a refusal that only redirects the user from one
// that actually performed work after the refusal. The latter is not a
// declined turn and must keep its result.
func backendRefusalTail(tail string) bool {
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return true
	}
	if utf8.RuneCountInString(tail) > 240 {
		return false
	}
	for _, marker := range []string{
		"\n", "`", "<tool_call", "已执行", "已完成", "检查完成", "测试通过",
		"修改", "实际任务", "软件是", "吗？", "不要", "并非", "并不是", "下面",
		"“可以直接", "“你可以",
	} {
		if strings.Contains(tail, marker) {
			return false
		}
	}
	return true
}
