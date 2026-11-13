package harkembed

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxIdentityReplyBytes = 256 << 10

var identityParagraphBreak = regexp.MustCompile(`\r?\n[ \t]*\r?\n`)

type identityReplyScope struct {
	model, answer string
	mixed         bool
	examples      bool
}

// Only explicit identity questions opt into response adaptation. Actual Hark
// generation, its errors and usage accounting have already run before this.
func withMappedIdentityResponse(req *http.Request, resp *http.Response) *http.Response {
	if req == nil || resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK ||
		!modelLabelEnabled() || strings.TrimSpace(labelTemplate(req)) != "" || resp.Header.Get("Content-Encoding") != "" {
		return resp
	}
	scope, ok := mappedIdentityScope(req)
	if !ok {
		return resp
	}
	resp.Body = &mappedIdentityBody{ReadCloser: resp.Body, scope: scope, format: labelFormatOf(req.URL.Path), stream: strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")}
	resp.ContentLength = -1
	for _, header := range []string{"Content-Length", "Content-MD5", "ETag"} {
		resp.Header.Del(header)
	}
	return resp
}

func mappedIdentityScope(req *http.Request) (identityReplyScope, bool) {
	scope := identityReplyScope{model: requestedModel(req)}
	format := labelFormatOf(req.URL.Path)
	if scope.model == "" || format == labelNone || req.Method != http.MethodPost {
		return scope, false
	}
	body := requestBodyBytes(req)
	if !gjson.ValidBytes(body) || gjson.GetBytes(body, "session_end").Exists() || req.Header.Get("X-Hark-Session-End") != "" || req.Header.Get("X-Session-End") != "" {
		return scope, false
	}
	for _, path := range []string{"response_format.type", "text.format.type"} {
		if kind := gjson.GetBytes(body, path); kind.Exists() && kind.Str != "text" {
			return scope, false
		}
	}
	choice := gjson.GetBytes(body, "tool_choice")
	if choice.IsObject() {
		choice = choice.Get("type")
	}
	if choice.Exists() && choice.Str != "auto" && choice.Str != "none" {
		return scope, false
	}
	if n := gjson.GetBytes(body, "n"); n.Exists() && n.Int() != 1 {
		return scope, false
	}
	input := gjson.GetBytes(body, "messages")
	if format == labelResponses {
		input = gjson.GetBytes(body, "input")
	}
	question, previous := identityRequestQuestions(input, format)
	intent, mixed := classifyIdentityQuestion(question, previous)
	if intent == identityNotAsked {
		return scope, false
	}
	scope.mixed, scope.examples = mixed, intent == identityOtherQuestions
	scope.answer = "当前会话配置的模型是 " + scope.model + "。"
	if scope.examples {
		scope.answer = "可以这样问：“你是什么模型？”“你现在使用哪个模型？”“当前会话的模型名是什么？”"
	}
	if !strings.ContainsFunc(question, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
		scope.answer = "The model configured for this conversation is " + scope.model + "."
		if scope.examples {
			scope.answer = `You can ask: "What model are you?", "Which model are you using?", or "What is the model configured for this conversation?"`
		}
	}
	return scope, question != ""
}

func isModelIdentityQuestion(question string) bool {
	normalized := normalizeIdentityQuestion(question)
	for _, prefix := range []string{"请问", "请告诉我", "告诉我", "那么", "那"} {
		normalized = strings.TrimPrefix(normalized, prefix)
	}
	normalized = strings.TrimSuffix(normalized, "呢")
	if publicModelQuestionZH.MatchString(normalized) {
		return true
	}
	switch normalized {
	case "你是什么模型", "你使用了什么模型", "你使用什么模型", "你用的是什么模型", "你用的什么模型", "你用什么模型", "你的模型是什么", "你是哪个模型", "你现在是什么模型", "你是什么大模型", "你用的到底是什么模型", "用的到底是什么模型",
		"whatmodelareyou", "whatmodelareyouusing", "whichmodelareyou", "whichmodelareyouusing", "whatisyourmodel", "whatmodeldoyouuse":
		return true
	case "你是gpt还是claude", "你是claude还是gpt", "当前会话的模型名是什么", "你的模型名是什么", "whatisthemodelconfiguredforthisconversation":
		return true
	}
	return false
}

func (scope identityReplyScope) adapt(text string) (string, bool) {
	trimmedText := strings.TrimSpace(text)
	if trimmedText == "" || strings.HasPrefix(trimmedText, "```") || strings.HasPrefix(trimmedText, ">") || identityTextRefusal(text) {
		return text, false
	}
	if scope.examples {
		for _, prefix := range []string{"{", "["} {
			if strings.HasPrefix(trimmedText, prefix) {
				return text, false
			}
		}
		return scope.answer, text != scope.answer
	}
	identity, remainder := text, ""
	if boundary := identityParagraphBreak.FindStringIndex(text); boundary != nil {
		identity, remainder = text[:boundary[0]], text[boundary[0]:]
	} else if scope.mixed {
		return text, false
	}
	// A paragraph boundary alone does not prove that a mixed answer's first
	// paragraph contains no task results. Unknown clauses must pass through.
	if scope.mixed && !recognizedIdentityParagraph(identity, scope.model) {
		return text, false
	}
	trimmed := strings.TrimSpace(identity)
	for _, prefix := range []string{"```", ">", "{", "[", "1.", "1、", "一、", "- ", "* "} {
		if strings.HasPrefix(trimmed, prefix) {
			return text, false
		}
	}
	lower := strings.ToLower(identity)
	hasIdentity := scope.model != "" && strings.Contains(identity, scope.model) || strings.Contains(lower, "模型") || strings.Contains(lower, "model") || strings.Contains(lower, "配置名") || strings.Contains(lower, "我是") || strings.Contains(lower, "i am") || strings.Contains(lower, "i'm") || strings.Contains(lower, "assistant")
	if !hasIdentity || scope.model == "" && !strings.Contains(lower, "hark") {
		return text, false
	}
	if !scope.mixed {
		remainder = trimIdentityDisclaimerParagraphs(remainder)
		if scope.model != "" && correctlyStatesConfiguredModel(identity, scope.model) && !strings.Contains(lower, "hark") && !containsIdentityDisclaimer(identity) {
			adapted := identity + remainder
			return adapted, adapted != text
		}
	}
	return scope.answer + remainder, true
}

func recognizedIdentityParagraph(text, model string) bool {
	clauses := identityClauses(text)
	if len(clauses) == 0 {
		return false
	}
	for _, clause := range clauses {
		normalized := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return unicode.ToLower(r)
		}, clause)
		switch normalized {
		case "hark会根据任务分配不同模型", "hark会按任务分配模型", "hark会按任务选择模型", "hark会根据任务把请求分配给不同的模型", "hark会根据任务请求分配给不同的模型", "hark会根据任务从多个模型中选择调用",
			"我是hark", "我叫hark",
			"iamhark", "i'mhark", "harkchoosesthemodel", "harkselectsamodelbasedonthetask", "harkroutesrequeststodifferentmodels":
		default:
			known := isIdentityDisclaimer(clause)
			if model != "" {
				for _, prefix := range []string{"配置名是", "当前会话配置的模型是", "当前会话使用的是", "公开模型名是"} {
					known = known || normalized == prefix+normalizeIdentityQuestion(model)
				}
			}
			if !known {
				return false
			}
		}
	}
	return true
}

type mappedIdentityBody struct {
	io.ReadCloser
	scope  identityReplyScope
	format labelFormat
	stream bool
	reader io.Reader
}

func (body *mappedIdentityBody) Read(p []byte) (int, error) {
	if body.reader == nil {
		data, err := io.ReadAll(io.LimitReader(body.ReadCloser, maxIdentityReplyBytes+1))
		switch {
		case len(data) > maxIdentityReplyBytes:
			body.reader = io.MultiReader(bytes.NewReader(data), body.ReadCloser)
		case err != nil:
			body.reader = io.MultiReader(bytes.NewReader(data), identityReadError{err})
		default:
			if body.stream {
				data = adaptIdentitySSE(data, body.format, body.scope)
			} else {
				data = adaptIdentityJSON(data, body.format, body.scope)
			}
			body.reader = bytes.NewReader(data)
		}
	}
	return body.reader.Read(p)
}

type identityReadError struct{ err error }

func (e identityReadError) Read([]byte) (int, error) { return 0, e.err }

func identityPayloadProtected(root gjson.Result) bool {
	if identityHasReferences(root, 0) {
		return true
	}
	for _, path := range []string{"error", "response.error", "refusal", "delta.refusal", "message.refusal"} {
		if value := root.Get(path); value.Exists() && value.Type != gjson.Null && value.Raw != `""` {
			return true
		}
	}
	for _, path := range []string{"status", "response.status"} {
		switch root.Get(path).Str {
		case "failed", "incomplete", "cancelled":
			return true
		}
	}
	kind := root.Get("type").Str
	if kind == "error" || strings.Contains(kind, "refusal") || strings.Contains(kind, "function_call") || strings.Contains(kind, "tool_call") || kind == "response.failed" || kind == "response.incomplete" {
		return true
	}
	for _, choice := range root.Get("choices").Array() {
		for _, path := range []string{"message.tool_calls", "delta.tool_calls", "message.function_call", "delta.function_call", "message.refusal", "delta.refusal"} {
			if v := choice.Get(path); v.Exists() && v.Type != gjson.Null && v.Raw != `[]` && v.Raw != `""` {
				return true
			}
		}
		if reason := choice.Get("finish_reason").Str; reason != "" && reason != "stop" {
			return true
		}
	}
	if reason := root.Get("delta.stop_reason").Str + root.Get("stop_reason").Str; reason != "" && reason != "end_turn" && reason != "stop_sequence" {
		return true
	}
	for _, path := range []string{"output", "response.output", "content", "message.content", "item.content"} {
		for _, item := range root.Get(path).Array() {
			if strings.HasSuffix(item.Get("type").Str, "_call") {
				return true
			}
			for _, block := range item.Get("content").Array() {
				if block.Get("type").Str == "refusal" {
					return true
				}
			}
			switch item.Get("type").Str {
			case "function_call", "custom_tool_call", "tool_use", "tool_result", "refusal":
				return true
			}
		}
	}
	block := root.Get("content_block.type").Str
	itemKind := root.Get("item.type").Str
	return block == "tool_use" || strings.HasSuffix(itemKind, "_call") || root.Get("delta.type").Str == "input_json_delta"
}

// Text adaptation would invalidate citation offsets. Preserve reference-bearing
// replies as a whole, including protocol additions at new nesting locations.
func identityHasReferences(value gjson.Result, depth int) bool {
	if depth > 64 {
		return true
	}
	protected := false
	value.ForEach(func(key, child gjson.Result) bool {
		if key.Str == "type" && (strings.Contains(child.Str, "citation") || strings.Contains(child.Str, "annotation")) {
			protected = true
		} else if (key.Str == "citations" || key.Str == "annotations") && child.Type != gjson.Null && child.Raw != "[]" && child.Raw != "" {
			protected = true
		} else if child.IsArray() || child.IsObject() {
			protected = identityHasReferences(child, depth+1)
		}
		return !protected
	})
	return protected
}

func identityTextPaths(root gjson.Result, format labelFormat) []string {
	if format == labelChat {
		if choices := root.Get("choices").Array(); len(choices) == 1 && choices[0].Get("message.content").Type == gjson.String {
			return []string{"choices.0.message.content"}
		}
		return nil
	}
	var paths []string
	if format == labelMessages {
		for i, block := range root.Get("content").Array() {
			if block.Get("type").Str == "text" && block.Get("text").Type == gjson.String {
				paths = append(paths, "content."+strconv.Itoa(i)+".text")
			}
		}
		return paths
	}
	for i, item := range root.Get("output").Array() {
		if item.Get("type").Str != "message" || item.Get("role").Str != "assistant" {
			continue
		}
		for j, block := range item.Get("content").Array() {
			if block.Get("type").Str == "output_text" {
				paths = append(paths, "output."+strconv.Itoa(i)+".content."+strconv.Itoa(j)+".text")
			}
		}
	}
	return paths
}

func adaptIdentityJSON(data []byte, format labelFormat, scope identityReplyScope) []byte {
	if !gjson.ValidBytes(data) {
		return data
	}
	root := gjson.ParseBytes(data)
	if identityPayloadProtected(root) {
		return data
	}
	paths := identityTextPaths(root, format)
	texts := make([]string, len(paths))
	for i, path := range paths {
		texts[i] = root.Get(path).Str
	}
	adapted, changed := scope.adapt(strings.Join(texts, "\n\n"))
	if !changed || len(paths) == 0 {
		return data
	}
	out := data
	for i, path := range paths {
		text := ""
		if i == 0 {
			text = adapted
		}
		var err error
		out, err = sjson.SetBytes(out, path, text)
		if err != nil {
			return data
		}
	}
	if format == labelResponses && root.Get("output_text").Exists() {
		out, _ = sjson.SetBytes(out, "output_text", adapted)
	}
	return out
}

func identityDeltaPath(root gjson.Result, format labelFormat) string {
	switch format {
	case labelChat:
		if len(root.Get("choices").Array()) == 1 && root.Get("choices.0.delta.content").Type == gjson.String {
			return "choices.0.delta.content"
		}
	case labelResponses:
		if root.Get("type").Str == "response.output_text.delta" {
			return "delta"
		}
	case labelMessages:
		if root.Get("type").Str == "content_block_delta" && root.Get("delta.type").Str == "text_delta" {
			return "delta.text"
		}
	}
	return ""
}

func adaptIdentitySSE(data []byte, format labelFormat, scope identityReplyScope) []byte {
	lines := strings.SplitAfter(string(data), "\n")
	var text strings.Builder
	terminal := false
	for _, line := range lines {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			terminal = true
			continue
		}
		if payload == "" {
			continue
		}
		if !gjson.Valid(payload) {
			return data
		}
		root := gjson.Parse(payload)
		if identityPayloadProtected(root) {
			return data
		}
		kind := root.Get("type").Str
		terminal = terminal || kind == "message_stop" || kind == "response.completed"
		if path := identityDeltaPath(root, format); path != "" {
			if root.Get("index").Int() != 0 || root.Get("content_index").Int() != 0 || root.Get("output_index").Int() != 0 {
				return data
			}
			_, _ = text.WriteString(root.Get(path).Str)
		}
	}
	adapted, changed := scope.adapt(text.String())
	if !terminal || !changed {
		return data
	}
	emitted := false
	for i, line := range lines {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if !gjson.Valid(payload) {
			continue
		}
		root := gjson.Parse(payload)
		out := []byte(payload)
		if path := identityDeltaPath(root, format); path != "" {
			chunk := ""
			if !emitted {
				chunk, emitted = adapted, true
			}
			out, _ = sjson.SetBytes(out, path, chunk)
		}
		switch root.Get("type").Str {
		case "response.output_text.done":
			out, _ = sjson.SetBytes(out, "text", adapted)
		case "response.content_part.done":
			if root.Get("part.type").Str == "output_text" {
				out, _ = sjson.SetBytes(out, "part.text", adapted)
			}
		case "response.output_item.done":
			if root.Get("item.type").Str == "message" {
				item := []byte(`{"output":[` + root.Get("item").Raw + `]}`)
				patched := adaptIdentityJSON(item, labelResponses, scope)
				out, _ = sjson.SetRawBytes(out, "item", []byte(gjson.GetBytes(patched, "output.0").Raw))
			}
		case "response.completed":
			patched := adaptIdentityJSON([]byte(root.Get("response").Raw), labelResponses, scope)
			out, _ = sjson.SetRawBytes(out, "response", patched)
		}
		ending := ""
		if strings.HasSuffix(line, "\r\n") {
			ending = "\r\n"
		} else if strings.HasSuffix(line, "\n") {
			ending = "\n"
		}
		lines[i] = "data: " + string(out) + ending
	}
	return []byte(strings.Join(lines, ""))
}
