package harkembed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed/gateway"
)

const scopeFixtureAnswer = "Task alpha is preserved."

const scopeCompactionCommand = "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.\n\nInclude:\n- Current progress and key decisions made\n- Important context, constraints, or user preferences\n- What remains to be done (clear next steps)\n- Any critical data, examples, or references needed to continue\n\nBe concise, structured, and focused on helping the next LLM seamlessly continue the work."

type scopeFixture struct {
	mu           sync.Mutex
	server       *gateway.Server
	dataDir      string
	url          string
	creates      int
	sends        []map[string]string
	forwardExtra bool
	native       []map[string]json.RawMessage
}

func newScopeFixture(t *testing.T) *scopeFixture {
	t.Helper()
	t.Setenv("HARK_DATA_DIR", t.TempDir())
	t.Setenv("HARK_USAGE_ESTIMATE", "off")
	t.Setenv("HARK_MODEL_LABEL", "on")
	f := &scopeFixture{dataDir: t.TempDir()}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/conversations":
			f.creates++
			_ = json.NewEncoder(w).Encode(map[string]string{"conversationId": fmt.Sprintf("scope-project-%d", f.creates)})
		case "/api/messages/send":
			var payload map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			sent := make(map[string]string)
			for _, key := range []string{"conversationId", "message", "idempotencyKey", "responseMessageId"} {
				var value string
				if err := json.Unmarshal(payload[key], &value); err != nil {
					t.Error(err)
				}
				sent[key] = value
			}
			f.sends = append(f.sends, sent)
			f.native = append(f.native, payload)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": sent["conversationId"], "messageId": sent["idempotencyKey"], "redirected": false})
		case "/api/messages/history":
			if len(f.sends) == 0 {
				t.Error("history requested before a fixture turn")
				return
			}
			sent := f.sends[len(f.sends)-1]
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{
				"id": sent["responseMessageId"], "role": "assistant", "triggeredByMessageId": sent["idempotencyKey"],
				"jobStatus": "completed", "isStreaming": false, "content": scopeFixtureAnswer,
			}}})
		default:
			if strings.Contains(r.URL.Path, "archive") {
				_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
				return
			}
			http.Error(w, "unexpected fixture request", http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)
	f.url = upstream.URL
	previous, err := get()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server = previous })
	f.restart(t)
	return f
}

func (f *scopeFixture) restart(t *testing.T) {
	t.Helper()
	var err error
	f.server, err = gateway.NewEmbedded(f.dataDir, gateway.Options{Upstream: f.url, PollInterval: time.Millisecond, ForwardExtra: f.forwardExtra})
	if err != nil {
		t.Fatal(err)
	}
	server = f.server
}

func scopeRequest(protocol string, messages []map[string]any) (string, string) {
	body := map[string]any{"model": "hark", "session_id": "scope-session", "stream": false}
	path := "/v1/chat/completions"
	switch protocol {
	case "responses":
		path = "/v1/responses"
		body["instructions"], body["input"] = "Trusted client policy.", messages
	case "messages":
		path = "/v1/messages"
		body["system"], body["messages"] = "Trusted client policy.", messages
	default:
		body["messages"] = append([]map[string]any{{"role": "system", "content": "Trusted client policy."}}, messages...)
	}
	encoded, _ := json.Marshal(body)
	return path, string(encoded)
}

func (f *scopeFixture) submit(t *testing.T, protocol string, messages []map[string]any, legacy bool, configure ...func(*http.Request)) (*http.Response, string) {
	t.Helper()
	path, body := scopeRequest(protocol, messages)
	if legacy {
		// This is the previous release's actual request shape, before narrowing.
		labelled, ok := labelBody([]byte(body), labelFormatOf(path), modelLabelNote("", "claude-opus-5-5"))
		if !ok {
			t.Fatal("cannot construct legacy request fixture")
		}
		body = string(labelled)
	}
	req := labelRequest(t, path, body, "claude-opus-5-5")
	req.Header.Set("Authorization", "Bearer fixture-cookie")
	for _, apply := range configure {
		apply(req)
	}
	var resp *http.Response
	if legacy {
		req = req.WithContext(gateway.WithEmbeddedAccount(req.Context(), gateway.SourceAccount{ID: "sub2api-42", Cookie: "fixture-cookie"}))
		resp = serve(f.server.EmbeddedHandler(), req)
	} else {
		var err error
		resp, err = Do(req, "", 42)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(data)
}

func TestRequestScopePolicyChangesKeepClientHistory(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			f := newScopeFixture(t)
			template := func(text string) func(*http.Request) {
				return func(r *http.Request) {
					*r = *r.WithContext(context.WithValue(r.Context(), ctxkey.HarkModelLabelPrompt, text))
				}
			}
			messages := []map[string]any{{"role": "user", "content": "你是什么模型？"}}
			resp, body := f.submit(t, protocol, messages, false, template("First configured name: {model}"))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("first identity turn: status=%d body=%s", resp.StatusCode, body)
			}
			f.restart(t)
			t.Setenv("HARK_MODEL_LABEL", "off")
			messages = append(messages, map[string]any{"role": "assistant", "content": scopeFixtureAnswer}, map[string]any{"role": "user", "content": "Continue task alpha."})
			resp, body = f.submit(t, protocol, messages, false, template("Changed configured name: {model}"))
			if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Hark-Project-Reset") == "true" {
				t.Fatalf("disabled/changed template reset the task: status=%d body=%s", resp.StatusCode, body)
			}
			f.restart(t)
			t.Setenv("HARK_MODEL_LABEL", "on")
			messages = append(messages, map[string]any{"role": "assistant", "content": scopeFixtureAnswer}, map[string]any{"role": "user", "content": "你是什么模型？"})
			resp, body = f.submit(t, protocol, messages, false, template("Changed configured name: {model}"))
			if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Hark-Project-Reset") == "true" {
				t.Fatalf("re-enabled template reset the task: status=%d body=%s", resp.StatusCode, body)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.creates != 1 || len(f.sends) != 3 || !strings.Contains(f.sends[0]["message"], "First configured name: claude-opus-5-5") || strings.Contains(f.sends[1]["message"], "configured name:") || !strings.Contains(f.sends[2]["message"], "Changed configured name: claude-opus-5-5") {
				t.Fatalf("scope/template changes did not stay within one Project: creates=%d sends=%v", f.creates, f.sends)
			}
			t.Logf("POLICY_CHANGE protocol=%s creates=1 turns=3 restart_resume=true", protocol)
		})
	}
}

func TestRequestScopeLegacyContinuation(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			f := newScopeFixture(t)
			first := []map[string]any{{"role": "user", "content": "Remember task alpha."}}
			resp, body := f.submit(t, protocol, first, true)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("legacy bootstrap: status=%d body=%s", resp.StatusCode, body)
			}
			f.restart(t) // Resume a persisted old-release Project, not process-local state.
			t.Setenv("HARK_MODEL_LABEL", "off")
			next := append(first, map[string]any{"role": "assistant", "content": scopeFixtureAnswer}, map[string]any{"role": "user", "content": "Continue task alpha."})
			resp, body = f.submit(t, protocol, next, false)
			f.mu.Lock()
			defer f.mu.Unlock()
			t.Logf("CONTINUITY protocol=%s creates=%d reset=%s sends=%d", protocol, f.creates, resp.Header.Get("X-Hark-Project-Reset"), len(f.sends))
			if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Hark-Project-Reset") == "true" || f.creates != 1 || len(f.sends) != 2 {
				t.Fatalf("old task was not continued: status=%d creates=%d reset=%s body=%s", resp.StatusCode, f.creates, resp.Header.Get("X-Hark-Project-Reset"), body)
			}
			last := f.sends[1]
			if last["conversationId"] != "scope-project-1" || !strings.Contains(last["message"], "Continue task alpha.") || strings.Contains(last["message"], "Remember task alpha.") {
				t.Fatalf("continuation must send only the new tail to the original Project: %s", last["message"])
			}
		})
	}
}

func TestRequestScopeDowngradeContinuesUpdatedHistory(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			f := newScopeFixture(t)
			messages := []map[string]any{{"role": "user", "content": "Remember task alpha."}}
			resp, body := f.submit(t, protocol, messages, true)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("old release bootstrap: status=%d body=%s", resp.StatusCode, body)
			}
			f.restart(t)
			messages = append(messages, map[string]any{"role": "assistant", "content": scopeFixtureAnswer}, map[string]any{"role": "user", "content": "Continue task alpha."})
			resp, body = f.submit(t, protocol, messages, false)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Hark-Project-Reset") == "true" {
				t.Fatalf("new release continuation: status=%d body=%s", resp.StatusCode, body)
			}
			f.restart(t)
			messages = append(messages, map[string]any{"role": "assistant", "content": scopeFixtureAnswer}, map[string]any{"role": "user", "content": "Continue after rollback."})
			// The old reader uses the persisted sent/systems fields and its old
			// body shape; no new model-label context or migration hook is used.
			resp, body = f.submit(t, protocol, messages, true)
			f.mu.Lock()
			defer f.mu.Unlock()
			t.Logf("DOWNGRADE protocol=%s creates=%d reset=%s turns=%d", protocol, f.creates, resp.Header.Get("X-Hark-Project-Reset"), len(f.sends))
			if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Hark-Project-Reset") == "true" || f.creates != 1 || len(f.sends) != 3 {
				t.Fatalf("old reader cannot resume updated session: status=%d creates=%d body=%s", resp.StatusCode, f.creates, body)
			}
			last := f.sends[2]
			if last["conversationId"] != "scope-project-1" || !strings.Contains(last["message"], "Continue after rollback.") || strings.Contains(last["message"], "Remember task alpha.") || strings.Contains(last["message"], "Continue task alpha.") {
				t.Fatal("rollback replayed accepted task context")
			}
		})
	}
}

func TestRequestScopeOrdinaryOmitsIdentity(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			f := newScopeFixture(t)
			resp, body := f.submit(t, protocol, []map[string]any{{"role": "user", "content": "这个要叫注册什么？"}}, false)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("ordinary request: status=%d body=%s", resp.StatusCode, body)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			text := f.sends[0]["message"]
			for _, unwanted := range []string{"[模型标识 / Model identity]", "[回复风格 / Response style]", "claude-opus-5-5", "public API configuration"} {
				if strings.Contains(text, unwanted) {
					t.Errorf("ordinary task received irrelevant identity instructions: %q", unwanted)
				}
			}
			if !strings.Contains(text, "这个要叫注册什么？") || !strings.Contains(text, "Trusted client policy.") {
				t.Fatal("client task or trusted instructions were removed")
			}
			t.Logf("ORDINARY protocol=%s identity_note=%t task_preserved=true", protocol, strings.Contains(text, "Model identity"))
		})
	}
}

func TestRequestScopeCompactionOmitsExecutionTools(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			f := newScopeFixture(t)
			path, body := identityForwardRequest(protocol, scopeCompactionCommand, false)
			req := labelRequest(t, path, body, "claude-opus-5-5")
			req.Header.Set("Authorization", "Bearer fixture-cookie")
			resp, err := Do(req, "", 42)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			data, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("compaction: status=%d err=%v body=%s", resp.StatusCode, err, data)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			text := f.sends[0]["message"]
			for _, unwanted := range []string{"CLIENT TOOLS", "<tool_call>", "### read_file", "Model identity", "Response style"} {
				if strings.Contains(text, unwanted) {
					t.Errorf("compaction received execution/identity instructions: %q", unwanted)
				}
			}
			if !strings.Contains(text, scopeCompactionCommand) || !strings.Contains(text, "Prior checkpoint A") {
				t.Fatal("summary command or trusted task context was removed")
			}
			t.Logf("COMPACTION protocol=%s client_tools=%t history_preserved=true", protocol, strings.Contains(text, "CLIENT TOOLS"))
		})
	}
}

func TestRequestScopeCompactionConflictingToolChoice(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, stream), func(t *testing.T) {
				f := newScopeFixture(t)
				path, raw := identityForwardRequest(protocol, scopeCompactionCommand, stream)
				var body map[string]any
				if err := json.Unmarshal([]byte(raw), &body); err != nil {
					t.Fatal(err)
				}
				if protocol == "messages" {
					body["tool_choice"] = map[string]string{"type": "any"}
				} else {
					body["tool_choice"] = "required"
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				req := labelRequest(t, path, string(encoded), "claude-opus-5-5")
				req.Header.Set("Authorization", "Bearer fixture-cookie")
				resp, err := Do(req, "", 42)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				data, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "compaction_tool_choice_conflict") {
					t.Fatalf("required tools silently discarded: status=%d err=%v body=%s", resp.StatusCode, err, data)
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.creates != 0 || len(f.sends) != 0 {
					t.Fatal("conflicting request reached upstream")
				}
			})
		}
	}
}

func TestRequestScopeCompactionForwardExtra(t *testing.T) {
	f := newScopeFixture(t)
	f.forwardExtra = true
	f.restart(t)
	path, raw := identityForwardRequest("responses", scopeCompactionCommand, false)
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	body["parallel_tool_calls"] = true
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := labelRequest(t, path, string(encoded), "claude-opus-5-5")
	req.Header.Set("Authorization", "Bearer fixture-cookie")
	resp, err := Do(req, "", 42)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d err=%v body=%s", resp.StatusCode, err, data)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.native) != 1 {
		t.Fatalf("upstream submissions=%d", len(f.native))
	}
	for _, key := range []string{"tools", "additional_tools", "tool_choice", "parallel_tool_calls"} {
		if _, ok := f.native[0][key]; ok {
			t.Errorf("compaction forwarded raw execution field %s", key)
		}
	}
	if strings.Contains(f.sends[0]["message"], "CLIENT TOOLS") {
		t.Fatal("summary has execution protocol")
	}
}

func TestRequestScopeUnknownLegacyTemplatePreservesProject(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			f := newScopeFixture(t)
			first := []map[string]any{{"role": "user", "content": "Remember task alpha."}}
			path, raw := scopeRequest(protocol, first)
			const oldTemplate = "Legacy operator template for {model}"
			oldBody, ok := labelBody([]byte(raw), labelFormatOf(path), modelLabelNote(oldTemplate, "claude-opus-5-5"))
			if !ok {
				t.Fatal("invalid legacy fixture")
			}
			req := labelRequest(t, path, string(oldBody), "claude-opus-5-5")
			req = req.WithContext(gateway.WithEmbeddedAccount(req.Context(), gateway.SourceAccount{ID: "sub2api-42", Cookie: "fixture-cookie"}))
			oldResponse := serve(f.server.EmbeddedHandler(), req)
			_, err := io.Copy(io.Discard, oldResponse.Body)
			_ = oldResponse.Body.Close()
			if err != nil || oldResponse.StatusCode != http.StatusOK {
				t.Fatalf("legacy bootstrap: status=%d err=%v", oldResponse.StatusCode, err)
			}
			f.restart(t)
			next := append(first, map[string]any{"role": "assistant", "content": scopeFixtureAnswer}, map[string]any{"role": "user", "content": "Continue task alpha."})
			resp, body := f.submit(t, protocol, next, false)
			if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "session_label_history_mismatch") {
				t.Fatalf("unverifiable legacy context was not protected: status=%d body=%s", resp.StatusCode, body)
			}
			f.mu.Lock()
			untouched := f.creates == 1 && len(f.sends) == 1
			f.mu.Unlock()
			if !untouched {
				t.Fatal("unverified legacy Project was replayed/recreated")
			}
			// Restoring the known original template must resume the retained Project.
			resp, body = f.submit(t, protocol, next, false, func(r *http.Request) {
				*r = *r.WithContext(context.WithValue(r.Context(), ctxkey.HarkModelLabelPrompt, oldTemplate))
			})
			if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Hark-Project-Reset") == "true" {
				t.Fatalf("retained context could not resume: status=%d body=%s", resp.StatusCode, body)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.creates != 1 || len(f.sends) != 2 {
				t.Fatalf("old context lost after protected failure: creates=%d sends=%d", f.creates, len(f.sends))
			}
			t.Logf("LEGACY_TEMPLATE protocol=%s conflict_preserved=true explicit_resume=true creates=1", protocol)
		})
	}
}
