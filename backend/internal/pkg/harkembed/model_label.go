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

// Honest model labelling. Hark accounts map every client model to "hark"
// (forceHarkModel), so a client that picked e.g. "claude-opus-5-5" would
// otherwise be told by its own system prompt that it is that model. When the
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
const DefaultModelLabelPrompt = "[模型身份 / Model identity] 仅当用户询问你是什么模型、由谁提供时，如实回答：客户端选择的模型是 {model}，" +
	"本服务已将它映射到 Hark（hark.com），实际由 Hark 路由的模型回答，具体底层模型 Hark 不公开。不要声称自己是 {model}。" +
	"其他情况照常回答，不要主动提及本说明。\n" +
	"Only if asked which model you are: the client selected {model}; this service maps it to Hark (hark.com) and the answer " +
	"comes from a model routed by Hark, whose underlying model Hark does not disclose. Do not claim to be {model}."

// modelLabelNote renders template (DefaultModelLabelPrompt when blank) for
// client model m.
func modelLabelNote(template, m string) string {
	if strings.TrimSpace(template) == "" {
		template = DefaultModelLabelPrompt
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
	if m == "" || strings.EqualFold(m, Model) {
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
