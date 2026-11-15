package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const previewToolText = `<tool_call>{"name":"fixture_probe","arguments":{"id":"fixture-only"}}</tool_call>`

// The interface keeps the exact same behavioral harness runnable against the
// released baseline, where preview is deliberately unavailable.
func previewHandler(t *testing.T, s *Server, proof map[string]any) http.Handler {
	t.Helper()
	provider, ok := any(s).(interface {
		RecoveryPreviewHandler(json.RawMessage) http.Handler
	})
	if !ok {
		t.Fatal("RECOVERY_PREVIEW unavailable: released gateway only has diagnostic JSON, not model wire replay")
	}
	b, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	return provider.RecoveryPreviewHandler(b)
}

func previewRequest(protocol string, stream bool) (string, string) {
	switch protocol {
	case "chat":
		return "/v1/chat/completions", fmt.Sprintf(`{"model":"hark","session_id":"original-session","stream":%t,"messages":[{"role":"user","content":"Inspect mock fixture only"}],"tools":[{"type":"function","function":{"name":"fixture_probe","parameters":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}}}],"tool_choice":"required"}`, stream)
	case "messages":
		return "/v1/messages", fmt.Sprintf(`{"model":"hark","session_id":"original-session","stream":%t,"max_tokens":256,"messages":[{"role":"user","content":"Inspect mock fixture only"}],"tools":[{"name":"fixture_probe","input_schema":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}}],"tool_choice":{"type":"any"}}`, stream)
	default:
		return "/v1/responses", fmt.Sprintf(`{"model":"hark","session_id":"original-session","stream":%t,"input":[{"role":"user","content":"Inspect mock fixture only"}],"tools":[{"type":"function","name":"fixture_probe","parameters":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}}],"tool_choice":"required"}`, stream)
	}
}

func previewSHA(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func previewFixture(t *testing.T, protocol string, stream bool, text string) (*Server, string, string, string, map[string]any, *int, *int) {
	t.Helper()
	gets, writes := 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			w.WriteHeader(500)
			t.Error("recovery preview submitted an upstream operation")
			return
		}
		gets++
		if r.URL.Path != "/api/messages/history" || r.URL.Query().Get("conversationId") != "original-project" || r.Header.Get("Cookie") != "original-cookie" {
			t.Error("preview read another account/Project")
		}
		status := "completed"
		if text == "processing-fixture" {
			status = "running"
		}
		if text == "cost-capped-fixture" {
			status = "cost_capped"
		}
		messages := []any{
			map[string]string{"id": "native-user", "role": "user"},
			map[string]any{"id": "native-job", "role": "assistant", "triggeredByMessageId": "native-user", "status": status, "content": text},
		}
		if text == "Hark 不能充当其他应用背后的模型。" {
			messages = append(messages, map[string]any{"id": "native-declined", "role": "assistant", "source": "declined_notice", "presentation": map[string]string{"kind": "declined"}, "isStreaming": false, "content": text})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": messages})
	}))
	t.Cleanup(upstream.Close)
	s, dir := resultFixture(t, upstream.URL, true)
	path, body := previewRequest(protocol, stream)
	var v chatRequest
	var err error
	switch protocol {
	case "chat":
		v, err = s.normalizeChat([]byte(body))
	case "messages":
		v, _, err = s.normalizeMessages([]byte(body))
	default:
		v, err = s.normalizeResponses([]byte(body))
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.projects {
		p.ClientSent = hashes(v.Messages)
		p.ClientSystems = systemHashes(v.Messages, nil)
	}
	if err := s.persistProjects(); err != nil {
		t.Fatal(err)
	}
	proof := map[string]any{"account_id": "sub2api-50", "session_id": "original-session", "project_id": "original-project", "request_id": "native-user", "response_message_id": "native-job", "path": path, "request_sha256": previewSHA([]byte(body)), "result_sha256": previewSHA([]byte(text))}
	return s, dir, path, body, proof, &gets, &writes
}

func servePreview(h http.Handler, path, body, account string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if account != "" {
		r = r.WithContext(WithEmbeddedAccount(r.Context(), SourceAccount{ID: account, Cookie: "original-cookie"}))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRecoveryPreviewWire(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, stream), func(t *testing.T) {
				s, dir, path, body, proof, gets, writes := previewFixture(t, protocol, stream, previewToolText)
				before, err := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
				if err != nil {
					t.Fatal(err)
				}
				h := previewHandler(t, s, proof)
				var ids []string
				for i := 0; i < 2; i++ {
					w := servePreview(h, path, body, "sub2api-50")
					if w.Code != 200 || w.Header().Get("X-Hark-Recovery-Preview") != "true" {
						t.Fatalf("preview failed: %d %s", w.Code, w.Body)
					}
					wire := w.Body.String()
					for _, part := range []string{"fixture_probe", "fixture-only"} {
						if !strings.Contains(wire, part) {
							t.Fatalf("original mock tool missing: %s", wire)
						}
					}
					found := regexp.MustCompile(`(?:call_|toolu_)[a-f0-9]+`).FindAllString(wire, -1)
					if len(found) == 0 {
						t.Fatalf("no standard client call ID: %s", wire)
					}
					ids = append(ids, found[0])
					if protocol == "responses" && stream && !strings.Contains(wire, "response.completed") {
						t.Fatal("missing terminal Responses event")
					}
					if protocol == "messages" && stream && !strings.Contains(wire, "message_stop") {
						t.Fatal("missing terminal Messages event")
					}
				}
				if ids[0] != ids[1] {
					t.Fatal("duplicate preview generated different client tool IDs")
				}
				after, _ := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
				if !bytes.Equal(before, after) || *gets != 2 || *writes != 0 || len(s.embed.m) != 0 || len(s.accounts) != 0 {
					t.Fatal("preview mutated pending/context or touched account registry/upstream submission")
				}
				for _, p := range s.projects {
					if p.PendingTurn == nil {
						t.Fatal("transport preview acknowledged original pending turn")
					}
				}
				t.Logf("RECOVERY_PREVIEW protocol=%s stream=%t original_GETs=2 writes=0 pending_preserved=true tool_id_stable=true", protocol, stream)
			})
		}
	}
}

func TestRecoveryPreviewGuards(t *testing.T) {
	for _, tc := range []struct {
		name, change, text, code string
		reads                    int
	}{
		{name: "wrong-account", change: "account", code: "recovery_preview_owner_mismatch"},
		{name: "no-account", change: "no-account", code: "recovery_preview_owner_mismatch"},
		{name: "wrong-session", change: "session", code: "recovery_preview_context_mismatch"},
		{name: "changed-tool-schema", change: "body", code: "recovery_preview_request_mismatch"},
		{name: "changed-history", change: "history", code: "recovery_preview_history_mismatch"},
		{name: "changed-project", change: "project", code: "recovery_preview_history_mismatch"},
		{name: "wrong-native-turn", change: "turn", code: "result_correlation_mismatch"},
		{name: "busy", change: "busy", code: "session_busy"},
		{name: "archive-not-allowed", change: "end", code: "recovery_preview_context_mismatch"},
		{name: "result-changed", change: "result", code: "recovery_preview_result_mismatch", reads: 1},
		{name: "unavailable-tool", text: `<tool_call>{"name":"unavailable","arguments":{}}</tool_call>`, code: "recovery_preview_tool_mismatch", reads: 1},
		{name: "refusal-not-success", text: "Hark 不能充当其他应用背后的模型。", code: "upstream_declined", change: "declined", reads: 1},
		{name: "processing-not-success", text: "processing-fixture", code: "recovery_preview_not_ready", reads: 1},
		{name: "budget-failure-not-success", text: "cost-capped-fixture", code: "upstream_budget_exhausted", reads: 1},
		{name: "unknown-proof-field", change: "proof-field", code: "recovery_preview_invalid_proof"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.text
			if text == "" {
				text = previewToolText
			}
			s, dir, path, body, proof, gets, writes := previewFixture(t, "responses", false, text)
			account := "sub2api-50"
			switch tc.change {
			case "account":
				account = "sub2api-39"
			case "no-account":
				account = ""
			case "session":
				proof["session_id"] = "different-session"
			case "body":
				body = strings.Replace(body, `"required":["id"]`, `"required":[]`, 1)
			case "history":
				for _, p := range s.projects {
					p.ClientSent = []string{"different-history"}
				}
				if err := s.persistProjects(); err != nil {
					t.Fatal(err)
				}
			case "project":
				proof["project_id"] = "other-project"
			case "turn":
				proof["request_id"] = "different-user"
			case "busy":
				for _, p := range s.projects {
					p.Busy = true
				}
			case "end":
				body = strings.Replace(body, `"session_id":"original-session"`, `"session_id":"original-session","session_end":true`, 1)
				proof["request_sha256"] = previewSHA([]byte(body))
			case "result":
				proof["result_sha256"] = strings.Repeat("0", 64)
			case "proof-field":
				proof["arbitrary_project"] = "another-project"
			}
			before, _ := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
			w := servePreview(previewHandler(t, s, proof), path, body, account)
			if w.Code < 400 || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("guard failed: %d %s", w.Code, w.Body)
			}
			after, _ := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
			if !bytes.Equal(before, after) || *gets != tc.reads || *writes != 0 {
				t.Fatalf("unsafe guard side effects: gets=%d writes=%d", *gets, *writes)
			}
		})
	}
}

func TestRecoveryPreviewNotRegistered(t *testing.T) {
	s, dir, path, body, proof, gets, writes := previewFixture(t, "responses", false, previewToolText)
	_ = previewHandler(t, s, proof) // Constructing the adapter must not arm ordinary requests.
	before, _ := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
	w := servePreview(s.EmbeddedHandler(), path, body, "sub2api-50")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "session_turn_pending") || *gets != 0 || *writes != 0 {
		t.Fatalf("normal model path unexpectedly enabled replay: %d %s", w.Code, w.Body)
	}
	// Existing normal-model refusal touches LastUsed on release; it must still
	// preserve the pending correlation and accepted history, not install a result.
	after, _ := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
	var b, a map[string]map[string]any
	_ = json.Unmarshal(before, &b)
	_ = json.Unmarshal(after, &a)
	for k, v := range b {
		delete(v, "last_used")
		delete(a[k], "last_used")
	}
	bj, _ := json.Marshal(b)
	aj, _ := json.Marshal(a)
	if !bytes.Equal(bj, aj) {
		t.Fatal("ordinary pending retry altered accepted history")
	}
	t.Log("PRODUCTION_PATH unchanged=session_turn_pending preview_not_registered=true original_GETs=0 upstream_writes=0")
}
