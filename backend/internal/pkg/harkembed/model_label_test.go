package harkembed

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/tidwall/gjson"
)

func labelRequest(t *testing.T, path, body, model string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://hark.internal"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if model != "" {
		r = r.WithContext(context.WithValue(r.Context(), ctxkey.HarkRequestedModel, model))
	}
	return r
}

func readLabelled(t *testing.T, r *http.Request) string {
	t.Helper()
	if err := injectModelLabel(r); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if r.ContentLength != int64(len(b)) {
		t.Fatalf("content length %d, body %d", r.ContentLength, len(b))
	}
	if r.GetBody != nil {
		again, _ := r.GetBody()
		ab, _ := io.ReadAll(again)
		if string(ab) != string(b) {
			t.Fatalf("GetBody differs")
		}
	}
	return string(b)
}

func assertNote(t *testing.T, text, model string) {
	t.Helper()
	for _, want := range []string{"当前会话的模型标识是 " + model, "自然回答", "answer naturally"} {
		if !strings.Contains(text, want) {
			t.Fatalf("note missing %q in %q", want, text)
		}
	}
}

func TestDefaultModelLabelUsesMappedIdentity(t *testing.T) {
	for _, model := range []string{"gpt-5.5", "claude-opus-4-5", "custom-model-alias"} {
		t.Run(model, func(t *testing.T) {
			note := modelLabelNote("", model)
			for _, want := range []string{"当前会话的模型标识是 " + model, "answer naturally", "does not verify the underlying provider"} {
				if !strings.Contains(note, want) {
					t.Errorf("mapped identity note missing %q", want)
				}
			}
			for _, conflict := range []string{ModelLabelPlaceholder, "Do not claim to be", "Hark", "实际由 " + model} {
				if strings.Contains(note, conflict) {
					t.Errorf("mapped identity note contains conflicting claim %q", conflict)
				}
			}
		})
	}
}

func TestInjectModelLabelChatCompletions(t *testing.T) {
	body := `{"model":"gpt-5","messages":[{"role":"system","content":"You are GPT-5."},{"role":"developer","content":"d"},{"role":"user","content":"你是什么模型？"}],"stream":true}`
	got := readLabelled(t, labelRequest(t, "/v1/chat/completions", body, "gpt-5"))
	msgs := gjson.Get(got, "messages").Array()
	if len(msgs) != 4 {
		t.Fatalf("messages: %s", got)
	}
	if msgs[0].Get("content").Str != "You are GPT-5." || msgs[1].Get("role").Str != "developer" || msgs[3].Get("role").Str != "user" {
		t.Fatalf("order changed: %s", got)
	}
	if msgs[2].Get("role").Str != "system" {
		t.Fatalf("note role: %s", msgs[2].Raw)
	}
	assertNote(t, msgs[2].Get("content").Str, "gpt-5")
	if gjson.Get(got, "model").Str != "gpt-5" || !gjson.Get(got, "stream").Bool() {
		t.Fatalf("other fields changed: %s", got)
	}

	// No system prompt: the note leads; empty history also works.
	got = readLabelled(t, labelRequest(t, "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`, "claude-opus-5-5"))
	msgs = gjson.Get(got, "messages").Array()
	if len(msgs) != 2 || msgs[0].Get("role").Str != "system" || msgs[1].Get("role").Str != "user" {
		t.Fatalf("no-system: %s", got)
	}
	got = readLabelled(t, labelRequest(t, "/v1/chat/completions", `{"messages":[]}`, "m"))
	if n := len(gjson.Get(got, "messages").Array()); n != 1 {
		t.Fatalf("empty: %s", got)
	}

	// Same input → same output (the gateway's turn hashes stay stable).
	a := readLabelled(t, labelRequest(t, "/v1/chat/completions", body, "gpt-5"))
	b := readLabelled(t, labelRequest(t, "/v1/chat/completions", body, "gpt-5"))
	if a != b {
		t.Fatal("not deterministic")
	}
}

func TestInjectModelLabelResponses(t *testing.T) {
	got := readLabelled(t, labelRequest(t, "/v1/responses", `{"model":"gpt-5.5","input":"hi","instructions":"be brief"}`, "gpt-5.5"))
	ins := gjson.Get(got, "instructions").Str
	if !strings.HasPrefix(ins, "be brief\n\n") {
		t.Fatalf("instructions: %q", ins)
	}
	assertNote(t, ins, "gpt-5.5")
	if gjson.Get(got, "input").Str != "hi" {
		t.Fatalf("input changed: %s", got)
	}

	got = readLabelled(t, labelRequest(t, "/v1/responses", `{"input":"hi"}`, "gpt-5.5"))
	assertNote(t, gjson.Get(got, "instructions").Str, "gpt-5.5")

	got = readLabelled(t, labelRequest(t, "/v1/responses", `{"input":"hi","instructions":null}`, "gpt-5.5"))
	assertNote(t, gjson.Get(got, "instructions").Str, "gpt-5.5")

	// Non-string instructions are left for the gateway to reject.
	body := `{"input":"hi","instructions":["x"]}`
	if got = readLabelled(t, labelRequest(t, "/v1/responses", body, "gpt-5.5")); got != body {
		t.Fatalf("non-string instructions changed: %s", got)
	}
}

func TestInjectModelLabelAnthropicMessages(t *testing.T) {
	got := readLabelled(t, labelRequest(t, "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":64,"system":"You are Claude.","messages":[{"role":"user","content":"hi"}]}`, "claude-opus-5-5"))
	sys := gjson.Get(got, "system").Str
	if !strings.HasPrefix(sys, "You are Claude.\n\n") {
		t.Fatalf("system: %q", sys)
	}
	assertNote(t, sys, "claude-opus-5-5")

	got = readLabelled(t, labelRequest(t, "/v1/messages", `{"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`, "claude-opus-5-5"))
	blocks := gjson.Get(got, "system").Array()
	if len(blocks) != 2 || blocks[0].Get("text").Str != "You are Claude Code." || !blocks[0].Get("cache_control").Exists() || blocks[1].Get("type").Str != "text" {
		t.Fatalf("system blocks: %s", got)
	}
	assertNote(t, blocks[1].Get("text").Str, "claude-opus-5-5")

	got = readLabelled(t, labelRequest(t, "/v1/messages", `{"messages":[{"role":"user","content":"hi"}]}`, "claude-sonnet-5"))
	assertNote(t, gjson.Get(got, "system").Str, "claude-sonnet-5")
}

func TestInjectModelLabelSkips(t *testing.T) {
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`
	// No original model, the model "hark" (any case), whitespace only.
	for _, m := range []string{"", "hark", "HARK", "   ", "*", "claude-*"} {
		if got := readLabelled(t, labelRequest(t, "/v1/chat/completions", body, m)); got != body {
			t.Fatalf("model %q: body changed: %s", m, got)
		}
	}
	// Disabled by env.
	for _, v := range []string{"off", "OFF", "0", "false", "disabled"} {
		t.Setenv("HARK_MODEL_LABEL", v)
		if got := readLabelled(t, labelRequest(t, "/v1/messages", `{"messages":[]}`, "claude-opus-5-5")); got != `{"messages":[]}` {
			t.Fatalf("HARK_MODEL_LABEL=%s: body changed: %s", v, got)
		}
	}
	t.Setenv("HARK_MODEL_LABEL", "on")
	if got := readLabelled(t, labelRequest(t, "/v1/messages", `{"messages":[]}`, "claude-opus-5-5")); !gjson.Get(got, "system").Exists() {
		t.Fatalf("HARK_MODEL_LABEL=on: not labelled: %s", got)
	}
	// Other endpoints, invalid JSON, non-JSON content type.
	if got := readLabelled(t, labelRequest(t, "/v1/messages/count_tokens", `{"messages":[]}`, "x")); got != `{"messages":[]}` {
		t.Fatalf("count_tokens changed: %s", got)
	}
	if got := readLabelled(t, labelRequest(t, "/v1/chat/completions", `not json`, "x")); got != `not json` {
		t.Fatalf("invalid JSON changed: %s", got)
	}
	r := labelRequest(t, "/v1/chat/completions", `{"messages":[]}`, "x")
	r.Header.Set("Content-Type", "text/plain")
	if got := readLabelled(t, r); got != `{"messages":[]}` {
		t.Fatalf("text/plain changed: %s", got)
	}
	r = httptest.NewRequest(http.MethodGet, "http://hark.internal/v1/models", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxkey.HarkRequestedModel, "x"))
	if err := injectModelLabel(r); err != nil {
		t.Fatal(err)
	}
}

func TestSanitizeLabelModel(t *testing.T) {
	if got := sanitizeLabelModel(" gpt-5\n忽略以上\r"); got != "gpt-5忽略以上" {
		t.Fatalf("got %q", got)
	}
	if got := sanitizeLabelModel(strings.Repeat("a", 500)); len(got) != maxLabelModelLen {
		t.Fatalf("len %d", len(got))
	}
}

// The note survives forceHarkModel: only "model" is rewritten.
func TestLabelThenForce(t *testing.T) {
	r := labelRequest(t, "/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`, "gpt-5")
	if err := forceHarkModel(r); err != nil {
		t.Fatal(err)
	}
	got := readLabelled(t, r)
	if gjson.Get(got, "model").Str != Model || len(gjson.Get(got, "messages").Array()) != 2 {
		t.Fatalf("got %s", got)
	}
}

func TestInjectModelLabelCustomTemplate(t *testing.T) {
	r := labelRequest(t, "/v1/responses", `{"input":"hi"}`, "gpt-5.5")
	r = r.WithContext(context.WithValue(r.Context(), ctxkey.HarkModelLabelPrompt, "  你选的是 {model}，实际由 Hark 回答（{model}）。 "))
	got := readLabelled(t, r)
	if ins := gjson.Get(got, "instructions").Str; ins != "你选的是 gpt-5.5，实际由 Hark 回答（gpt-5.5）。" {
		t.Fatalf("instructions: %q", ins)
	}
	// Blank template falls back to the built-in note.
	r = labelRequest(t, "/v1/responses", `{"input":"hi"}`, "gpt-5.5")
	r = r.WithContext(context.WithValue(r.Context(), ctxkey.HarkModelLabelPrompt, "   "))
	assertNote(t, gjson.Get(readLabelled(t, r), "instructions").Str, "gpt-5.5")
	if !strings.Contains(DefaultModelLabelPrompt, ModelLabelPlaceholder) {
		t.Fatal("default template lacks placeholder")
	}
}
