package gateway

import (
	"strings"
	"unicode/utf8"
)

// These are observed plain-text backend refusals, not native declined_notice
// metadata. Match only an explicit self-refusal at the start of a short,
// unquoted, finished text bubble. Do not
// generalize "sorry", provider mentions or quoted refusal examples into errors.
// Unknown wording intentionally remains ordinary text until evidenced.
func recordedBackendRefusal(m harkMessage) bool {
	if m.Source != "send_bubble" || m.Presentation.Kind != "text" || m.Streaming == nil || *m.Streaming {
		return false
	}
	text := strings.TrimSpace(m.Content)
	if utf8.RuneCountInString(text) > 512 || strings.ContainsAny(text, "\r\n`\"“”") {
		return false
	}
	// Observed production bubbles use several other wordings. Match their
	// complete text, not arbitrary refusal keywords or an unbounded suffix.
	switch text {
	case "Hark 不能当作其他应用背后的模型来用。你想做成什么事可以直接告诉我，我很乐意帮忙。",
		"Hark 不能当作别的应用背后的模型来用。你想做成什么事，直接告诉我，我很乐意帮忙。",
		"我没法充当其他应用背后的模型，你可以直接告诉我想做成什么，我会在这里帮你。",
		"Hark 不能被当作其他应用背后的模型来调用。如果你告诉我你实际想完成什么，我很乐意直接在这里帮你。",
		"Hark 不能作为其他应用背后的模型来使用。如果你告诉我你想做成什么，我很乐意直接帮你。",
		"Hark 不支持作为其他应用背后的模型来使用。如果你告诉我你想完成什么，我很乐意直接在这里帮你。",
		"Hark 不能用作其他应用背后的模型。不过如果你告诉我你想完成什么，我很乐意直接在这里帮你。":
		return true
	}
	for _, prefix := range []string{
		"抱歉，Hark 不支持作为其他应用背后的模型来使用。",
		"抱歉，Hark 不能作为其他应用背后的模型来使用。",
		"这条请求还是另一个应用打包好、交给它自己模型的完整请求，Hark 不支持充当那个模型；",
	} {
		if tail, ok := strings.CutPrefix(text, prefix); ok && (tail == "" || strings.HasPrefix(tail, "如果你")) {
			return true
		}
	}
	return false
}
