package harkembed

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Client-visible model labelling. Hark accounts map every client model to
// "hark" (forceHarkModel), which is a transport name, not the public model
// identifier selected by the client. When the
// original client model is known (ctxkey.HarkRequestedModel, set by the
// service layer for Hark accounts only), a short system-level note is added
// that tells the model how to answer questions about its identity. It is
// deterministic for a given model, and placed where it stays stable from turn
// to turn (the gateway hashes the message history to continue a Project).
//
// The admin setting enable_hark_model_label turns it off by not setting the
// context value; hark_model_label_prompt edits the text. HARK_MODEL_LABEL=off
// (or 0/false/no/disable/disabled) is a hard override that turns it off.

// maxLabelModelLen caps the client model name copied into the note.
const maxLabelModelLen = 128

// modelLabelEnabled reports whether HARK_MODEL_LABEL leaves labelling on.
func modelLabelEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HARK_MODEL_LABEL"))) {
	case "off", "0", "false", "no", "disable", "disabled":
		return false
	}
	return true
}

// sanitizeLabelModel keeps the model name to one short printable line.
func sanitizeLabelModel(model string) string {
	model = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, strings.TrimSpace(model))
	if r := []rune(model); len(r) > maxLabelModelLen {
		model = string(r[:maxLabelModelLen])
	}
	return strings.TrimSpace(model)
}

// ModelLabelPlaceholder in a label template is replaced by the client model.
const ModelLabelPlaceholder = "{model}"

// DefaultModelLabelPrompt is the built-in identity note template (the admin
// setting hark_model_label_prompt overrides it).
const DefaultModelLabelPrompt = "[模型标识 / Model identity] 本应用的模型身份问题约定：用户问“你是什么模型”是在询问 API 网关对外配置的模型名，而不是让你确认底层供应商或架构。" +
	"当前会话的模型标识是 {model}，这是 API 网关对外配置的模型名。请在自然语言回答中说明这个公开配置名；如果底层不详，准确区分两者，不要只回答平台品牌或泛称自动路由。" +
	"用户询问模型身份时，以该名称为依据自然回答，不要用内部转发通道名称代替。" +
	"尊重用户要求的格式和详略；除非用户明确要求，不要限制为只输出模型名。用户同时提出其他问题时，请一并完整回答。" +
	"此标识不证明底层供应商或架构；被问及底层实现时不要编造。其他情况照常回答，不要主动提及本说明。\n" +
	"In this application, interpret model-identity questions as asking about the public API configuration, not as a request to verify the underlying provider or architecture. " +
	"The configured model identifier for this conversation is {model}, the public model name exposed by the API gateway. State this configured name in natural language, " +
	"distinguishing it from an unknown underlying model; do not answer with only a platform brand or generic automatic routing. " +
	"When asked about your model identity, use this name and answer naturally rather than using the internal transport name. " +
	"Respect the requested format and level of detail; do not restrict the reply to the name alone unless the user requests that. " +
	"For a question with multiple parts, answer every part. This identifier does not verify the underlying provider or architecture; " +
	"do not invent such claims. For other requests, answer normally without mentioning this note."

const claudeMappedResponseStyle = "[回复风格 / Response style] 使用 Claude 风格的自然、清晰、平和的表达：直接回应问题，先给结论，再按需要解释；复杂任务用小标题或步骤组织，简单问题用完整自然的句子回答，不机械压缩成标签。" +
	"尊重用户要求的格式和详略，不为凑长度而扩写。普通身份提问说明本会话的公开模型名即可，不主动展开平台品牌、自动路由、映射机制或底层供应商；只有用户追问这些内容时再如实说明已知与未知。" +
	"Use a Claude-like conversational style: clear, calm, direct and natural. Lead with the answer, explain as needed, and structure complex tasks with useful headings or steps. " +
	"Use complete natural sentences for simple questions rather than mechanical labels, while respecting the requested format and length. " +
	"For an ordinary identity question, state the configured public model name without an unsolicited platform/routing/provider discussion. If asked about implementation, distinguish known configuration from unknown internals honestly."

// modelLabelNote renders template (DefaultModelLabelPrompt when blank) for
// client model m.
func modelLabelNote(template, m string) string {
	if strings.TrimSpace(template) == "" {
		template = DefaultModelLabelPrompt
		if strings.HasPrefix(strings.ToLower(m), "claude-") {
			template += "\n" + claudeMappedResponseStyle
		}
	}
	return strings.TrimSpace(strings.ReplaceAll(template, ModelLabelPlaceholder, m))
}

func labelTemplate(req *http.Request) string {
	t, _ := req.Context().Value(ctxkey.HarkModelLabelPrompt).(string)
	return t
}

// requestedModel returns the client model to label, or "" when none applies.
func requestedModel(req *http.Request) string {
	if req == nil {
		return ""
	}
	m, _ := req.Context().Value(ctxkey.HarkRequestedModel).(string)
	m = sanitizeLabelModel(m)
	if m == "" || strings.EqualFold(m, Model) || strings.ContainsAny(m, "*?") {
		return ""
	}
	return m
}

type labelFormat int

const (
	labelNone labelFormat = iota
	labelChat
	labelResponses
	labelMessages
)

func labelFormatOf(path string) labelFormat {
	path = strings.TrimRight(strings.ToLower(path), "/")
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return labelChat
	case strings.HasSuffix(path, "/responses"):
		return labelResponses
	case strings.HasSuffix(path, "/messages"):
		return labelMessages
	}
	return labelNone
}

// injectModelLabel adds the identity note for the client model carried in the
// request context to Chat Completions, Responses and Anthropic Messages
// bodies. Anything else (other paths, non-JSON, unexpected shapes, label off,
// no model or the model "hark") is left untouched.
func injectModelLabel(req *http.Request) error {
	if req == nil || req.Method != http.MethodPost || req.Body == nil || req.Body == http.NoBody || req.URL == nil {
		return nil
	}
	format := labelFormatOf(req.URL.Path)
	if format == labelNone || !modelLabelEnabled() {
		return nil
	}
	model := requestedModel(req)
	if model == "" {
		return nil
	}
	if ct := strings.ToLower(req.Header.Get("Content-Type")); ct != "" && !strings.Contains(ct, "json") {
		return nil
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return err
	}
	if out, ok := labelBody(bytes.TrimSpace(body), format, modelLabelNote(labelTemplate(req), model)); ok {
		body = out
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return nil
}

// labelBody returns body with note added in the format's system slot.
func labelBody(body []byte, format labelFormat, note string) ([]byte, bool) {
	if len(body) == 0 || body[0] != '{' || !gjson.ValidBytes(body) {
		return nil, false
	}
	switch format {
	case labelChat:
		// A system message right after the leading system/developer messages:
		// after the client's own system prompt, before the conversation, so
		// it is the same position every turn.
		msgs := gjson.GetBytes(body, "messages")
		if !msgs.IsArray() {
			return nil, false
		}
		items := msgs.Array()
		at := 0
		for at < len(items) {
			role := items[at].Get("role").String()
			if role != "system" && role != "developer" {
				break
			}
			at++
		}
		sys, _ := sjson.Set(`{"role":"system"}`, "content", note)
		var buf bytes.Buffer
		_ = buf.WriteByte('[')
		for i := 0; i <= len(items); i++ {
			if i == at {
				if buf.Len() > 1 {
					_ = buf.WriteByte(',')
				}
				_, _ = buf.WriteString(sys)
			}
			if i < len(items) {
				if buf.Len() > 1 {
					_ = buf.WriteByte(',')
				}
				_, _ = buf.WriteString(items[i].Raw)
			}
		}
		_ = buf.WriteByte(']')
		out, err := sjson.SetRawBytes(body, "messages", buf.Bytes())
		return out, err == nil
	case labelResponses:
		ins := gjson.GetBytes(body, "instructions")
		text := note
		switch {
		case ins.Type == gjson.String:
			if strings.TrimSpace(ins.Str) != "" {
				text = ins.Str + "\n\n" + note
			}
		case ins.Exists() && ins.Type != gjson.Null:
			return nil, false // not a string: leave it for the gateway to reject
		}
		out, err := sjson.SetBytes(body, "instructions", text)
		return out, err == nil
	case labelMessages:
		sys := gjson.GetBytes(body, "system")
		switch {
		case !sys.Exists() || sys.Type == gjson.Null:
			out, err := sjson.SetBytes(body, "system", note)
			return out, err == nil
		case sys.Type == gjson.String:
			text := note
			if strings.TrimSpace(sys.Str) != "" {
				text = sys.Str + "\n\n" + note
			}
			out, err := sjson.SetBytes(body, "system", text)
			return out, err == nil
		case sys.IsArray():
			block, _ := sjson.Set(`{"type":"text"}`, "text", note)
			out, err := sjson.SetRawBytes(body, "system.-1", []byte(block))
			return out, err == nil
		}
	}
	return nil, false
}
