package harkembed

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/tidwall/gjson"
)

type modelIdentityIntent uint8

const (
	identityNotAsked modelIdentityIntent = iota
	identityPublicName
	identityOtherQuestions
)

var publicModelQuestionZH = regexp.MustCompile(`^你(?:现在|当前)?(?:使用|用)(?:的)?(?:是)?(?:什么|哪个|哪种)模型(?:呢|吗)?$`)
var otherModelQuestionsZH = regexp.MustCompile(`^(?:问|询问)(?:你)?(?:使用|用)?的?(?:什么|哪个)?模型(?:身份)?(?:还有|有|有没有)(?:哪些|什么|其他|其它|别的)(?:问法|提问方式)(?:吗|呢)?$`)
var otherModelQuestionsEN = regexp.MustCompile(`^(?:whatare|arethere)(?:other|someother|different|alternative)waystoask(?:which|what)model(?:youuse|youareusing|youare)$`)

func normalizeIdentityQuestion(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || strings.ContainsRune("?？!！.。", r) {
			return -1
		}
		return unicode.ToLower(r)
	}, text)
}

// Only explicit questions enter normalization. Provider/developer questions
// remain upstream answers; a public alias cannot establish those facts.
func asksIdentityInternals(question string) bool {
	question = normalizeIdentityQuestion(question)
	for _, marker := range []string{"底层", "真实", "实际", "供应商", "厂商", "开发", "映射", "路由", "underlying", "provider", "developer", "developed", "created", "routing", "mapping", "actual"} {
		if strings.Contains(question, marker) {
			return true
		}
	}
	return false
}

func asksOtherModelQuestions(question string) bool {
	normalized := normalizeIdentityQuestion(question)
	return otherModelQuestionsZH.MatchString(normalized) || otherModelQuestionsEN.MatchString(normalized) ||
		normalized == "怎么询问你使用的模型" || normalized == "如何询问你是什么模型"
}

func classifyIdentityQuestion(question, previous string) (modelIdentityIntent, bool) {
	if asksIdentityInternals(question) {
		return identityNotAsked, false
	}
	if asksOtherModelQuestions(question) {
		return identityOtherQuestions, false
	}
	contextual := !asksIdentityInternals(previous) && (isModelIdentityQuestion(previous) || asksOtherModelQuestions(previous))
	if contextual {
		switch normalizeIdentityQuestion(question) {
		case "还有其他问法吗", "还有其它问法吗", "还有哪些问法", "anyotherwaystoask", "anyotherwaystoaskthis":
			return identityOtherQuestions, false
		case "你是哪个版本", "哪个版本", "whichversionareyou":
			return identityPublicName, false
		}
	}
	if isModelIdentityQuestion(question) {
		return identityPublicName, false
	}
	// Mixed answers are still only adapted when a separate, known identity
	// paragraph can be identified without discarding another task's result.
	if at := strings.IndexAny(question, "?？。\n,，"); at >= 0 && isModelIdentityQuestion(question[:at]) {
		return identityPublicName, true
	}
	return identityNotAsked, false
}

func identityMessageText(message gjson.Result) string {
	content := message.Get("content")
	if content.Type == gjson.String {
		return content.Str
	}
	if parts := content.Array(); len(parts) == 1 && (parts[0].Get("type").Str == "text" || parts[0].Get("type").Str == "input_text") {
		return parts[0].Get("text").Str
	}
	return ""
}

func identityRequestQuestions(input gjson.Result, format labelFormat) (string, string) {
	if format == labelResponses && input.Type == gjson.String {
		return input.Str, ""
	}
	items := input.Array()
	if !input.IsArray() || len(items) == 0 || items[len(items)-1].Get("role").Str != "user" {
		return "", ""
	}
	question := identityMessageText(items[len(items)-1])
	for i := len(items) - 2; i >= 0; i-- {
		if items[i].Get("role").Str == "user" {
			return question, identityMessageText(items[i])
		}
	}
	return question, ""
}

func isIdentityDisclaimer(clause string) bool {
	switch normalizeIdentityQuestion(clause) {
	case "底层实际是哪个模型我确认不了", "底层实际模型我确认不了", "底层模型未知", "底层模型我不清楚",
		"我无法确认当前实际模型", "我无法确认实际模型", "我无法确认当前具体是哪个模型在运行", "我没法确切知道这次用的是哪一个", "我没法确切知道现在用的是哪一个",
		"现在具体在用哪个模型", "我自己也看不到", "icannotconfirmtheunderlyingmodel", "theunderlyingmodelisunknown":
		return true
	}
	return false
}

func identityClauses(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool { return strings.ContainsRune("，,。.;；?？!！\r\n", r) })
}

func containsIdentityDisclaimer(text string) bool {
	for _, clause := range identityClauses(text) {
		if isIdentityDisclaimer(clause) {
			return true
		}
	}
	return false
}

func trimIdentityDisclaimerParagraphs(text string) string {
	parts := identityParagraphBreak.Split(text, -1)
	separators := identityParagraphBreak.FindAllString(text, -1)
	out := parts[0]
	for i := 1; i < len(parts); i++ {
		clauses := identityClauses(parts[i])
		disclaimer := len(clauses) > 0
		for _, clause := range clauses {
			disclaimer = disclaimer && isIdentityDisclaimer(clause)
		}
		if !disclaimer {
			out += separators[i-1] + parts[i]
		}
	}
	return out
}

func identityTextRefusal(text string) bool {
	text = strings.TrimSpace(strings.ToLower(text))
	for _, prefix := range []string{"抱歉，", "抱歉,", "对不起，", "sorry,", "i'm sorry,", "i am sorry,"} {
		text = strings.TrimSpace(strings.TrimPrefix(text, prefix))
	}
	for _, prefix := range []string{"我不能披露", "我无法披露", "我不能透露", "我无法透露", "我不能提供", "我无法提供", "我不能协助", "我不能帮助", "i cannot disclose", "i can't disclose", "i cannot reveal", "i can't reveal", "i cannot help", "i can't help", "i can't assist", "i cannot assist"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// A mention or substring of an alias is not a positive identity assertion.
// Require a positive prefix and an exact name with a sentence boundary.
func correctlyStatesConfiguredModel(text, model string) bool {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{"配置名是 ", "当前会话配置的模型是 ", "当前会话使用的是 ", "当前会话的模型配置是 ", "The model configured for this conversation is "} {
		if rest, ok := strings.CutPrefix(text, prefix+model); ok {
			if rest == "" {
				return true
			}
			if rest == "." || strings.HasPrefix(rest, ". ") || strings.HasPrefix(rest, ".\n") || strings.HasPrefix(rest, ".\r") {
				return true
			}
			for _, boundary := range []string{"。", "，", ",", "；", ";", "\n"} {
				if strings.HasPrefix(rest, boundary) {
					return true
				}
			}
		}
	}
	return false
}
