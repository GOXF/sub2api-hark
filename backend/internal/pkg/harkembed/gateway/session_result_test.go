package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionResultReadOnly(t *testing.T) {
	const originalText = `<tool_call>{"name":"exec_command","arguments":{"cmd":"echo not-executed"}}</tool_call>`
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-explicit-correlation", true: "persisted-pending-correlation"}[pending], func(t *testing.T) {
			gets, writes := 0, 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					t.Error("read-only recovery issued an upstream write")
					w.WriteHeader(500)
					return
				}
				gets++
				if r.URL.Path != "/api/messages/history" || r.URL.Query().Get("conversationId") != "original-project" || r.Header.Get("Cookie") != "original-cookie" || r.UserAgent() != "original-agent" {
					t.Error("recovery read another project/account or lost its browser identity")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{
					map[string]any{"id": "native-user", "role": "user", "content": "original task"},
					map[string]any{"id": "native-job", "role": "assistant", "triggeredByMessageId": "native-user", "status": "completed"},
					map[string]any{"id": "native-bubble", "role": "assistant", "triggeredByMessageId": "native-user", "source": "send_bubble", "presentation": map[string]string{"kind": "text"}, "status": "completed", "content": originalText},
				}})
			}))
			defer upstream.Close()
			s, dir := resultFixture(t, upstream.URL, pending)
			before, err := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			query := "?request_id=native-user&response_message_id=native-job"
			if pending {
				query = ""
			}
			for i := 0; i < 2; i++ {
				req := httptest.NewRequest(http.MethodGet, "/v1/sessions/original-session/result"+query, nil)
				req = req.WithContext(WithEmbeddedAccount(req.Context(), SourceAccount{ID: "sub2api-50", Cookie: "original-cookie", UserAgent: "original-agent"}))
				rec := httptest.NewRecorder()
				s.EmbeddedHandler().ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Fatalf("read-only result unavailable: status=%d body=%s", rec.Code, rec.Body)
				}
				var got map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				result, _ := got["result"].(map[string]any)
				if got["read_only"] != true || got["continuation_applied"] != false || got["status"] != "awaiting_client_tool" || result["text"] != originalText || got["request_id"] != "native-user" || got["response_message_id"] != "native-job" {
					t.Fatalf("tool result must remain unexecuted diagnostic text: %s", rec.Body)
				}
				if _, ok := got["tool_calls"]; ok {
					t.Fatal("recovery exposed executable model tool_calls")
				}
			}
			after, err := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("read-only recovery changed original session bytes")
			}
			memory, _ := json.Marshal(s.projects)
			if !bytes.Equal(before, memory) || len(s.accounts) != 0 || len(s.embed.m) != 0 {
				t.Fatal("read-only recovery changed session/account registry state")
			}
			if gets != 2 || writes != 0 {
				t.Fatalf("gets=%d upstream_writes=%d", gets, writes)
			}
			t.Logf("READ_ONLY status=awaiting_client_tool GETs=%d writes=%d context_unchanged=true continuation_applied=false", gets, writes)
		})
	}
}

func resultFixture(t *testing.T, upstream string, pending bool) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	key := sha256.Sum256([]byte("embed:sub2api-50\x00original-session"))
	id := hex.EncodeToString(key[:])
	p := &projectSession{MappingID: id, AccountID: "sub2api-50", ConversationID: "original-project", Turns: 3, LabelHistoryVersion: 2, ClientSent: []string{"original-history-fingerprint"}, LastUsed: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	if pending {
		p.PendingTurn = &pendingProjectTurn{RequestID: "native-user", Expected: "native-job"}
	}
	b, _ := json.Marshal(map[string]*projectSession{id: p})
	if err := os.WriteFile(filepath.Join(dir, "project-sessions.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewEmbedded(dir, Options{Upstream: upstream})
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestSessionResultIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, method, account, path, query, code string
		busy, pending                            bool
		status                                   int
	}{
		{name: "unknown-session", path: "unknown", account: "sub2api-50", status: 404, code: "session_not_found"},
		{name: "wrong-account", account: "sub2api-39", status: 404, code: "session_not_found"},
		{name: "no-embedded-auth", account: "", status: 401, code: "invalid_api_key"},
		{name: "standalone-api-cannot-read", account: "standalone", status: 401, code: "embedded_account_required"},
		{name: "no-legacy-selector", account: "sub2api-50", status: 400, code: "result_correlation_required"},
		{name: "one-selector", account: "sub2api-50", query: "request_id=native-user", status: 400, code: "invalid_result_selector"},
		{name: "caller-project", account: "sub2api-50", query: "conversationId=another-project", status: 400, code: "invalid_result_selector"},
		{name: "duplicate-selector", account: "sub2api-50", query: "request_id=x&request_id=y&response_message_id=z", status: 400, code: "invalid_result_selector"},
		{name: "malformed-query", account: "sub2api-50", query: "request_id=%zz&response_message_id=native-job", status: 400, code: "invalid_result_selector"},
		{name: "empty-selector", account: "sub2api-50", query: "request_id=&response_message_id=", pending: true, status: 400, code: "invalid_result_selector"},
		{name: "pending-mismatch", account: "sub2api-50", pending: true, query: "request_id=other-user&response_message_id=other-job", status: 409, code: "result_correlation_mismatch"},
		{name: "busy", account: "sub2api-50", pending: true, busy: true, status: 409, code: "session_busy"},
		{name: "post-not-supported", account: "sub2api-50", method: http.MethodPost, pending: true, status: 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				t.Error("invalid selector/auth/method issued an upstream request")
			}))
			defer upstream.Close()
			s, dir := resultFixture(t, upstream.URL, tc.pending)
			if tc.busy {
				for _, p := range s.projects {
					p.Busy = true
				}
			}
			before, _ := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
			if tc.method == "" {
				tc.method = http.MethodGet
			}
			if tc.path == "" {
				tc.path = "original-session"
			}
			req := httptest.NewRequest(tc.method, "/v1/sessions/"+tc.path+"/result?"+tc.query, nil)
			if tc.account != "" && tc.account != "standalone" {
				req = req.WithContext(WithEmbeddedAccount(req.Context(), SourceAccount{ID: tc.account, Cookie: "original-cookie"}))
			}
			rec := httptest.NewRecorder()
			if tc.account == "standalone" {
				s.Handler().ServeHTTP(rec, req)
			} else {
				s.EmbeddedHandler().ServeHTTP(rec, req)
			}
			if rec.Code != tc.status || tc.code != "" && !bytes.Contains(rec.Body.Bytes(), []byte(tc.code)) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			after, _ := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
			if !bytes.Equal(before, after) || calls != 0 || len(s.accounts) != 0 || len(s.embed.m) != 0 {
				t.Fatal("negative read changed state or called upstream")
			}
		})
	}
}

func TestSessionResultStates(t *testing.T) {
	for _, tc := range []struct {
		name, jobStatus, bubble, resultStatus, errorCode        string
		inputNeeded, declinedNotice, foreign, duplicate, mutate bool
		upstreamStatus                                          int
	}{
		{name: "completed", jobStatus: "completed", bubble: "original answer", resultStatus: "completed"},
		{name: "processing", jobStatus: "running", resultStatus: "processing"},
		{name: "waiting", jobStatus: "completed", bubble: "Please supply the file", inputNeeded: true, resultStatus: "awaiting_input"},
		{name: "refusal-preserved", jobStatus: "completed", bubble: "Original native refusal", declinedNotice: true, resultStatus: "declined"},
		{name: "budget-stop", jobStatus: "cost_capped", bubble: "Partial original content", resultStatus: "failed", errorCode: "upstream_budget_exhausted"},
		{name: "generation-failed", jobStatus: "failed", resultStatus: "failed", errorCode: "upstream_generation_failed"},
		{name: "rate-limit-no-retry", upstreamStatus: 429, errorCode: "upstream_rate_limited"},
		{name: "auth-failure-no-rotation", upstreamStatus: 401, errorCode: "upstream_session_expired"},
		{name: "foreign-job", jobStatus: "completed", foreign: true, errorCode: "result_correlation_unverified"},
		{name: "duplicate-native-id", jobStatus: "completed", duplicate: true, errorCode: "result_correlation_unverified"},
		{name: "mapping-changed-during-get", jobStatus: "completed", bubble: "original answer", mutate: true, errorCode: "session_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, writes := 0, 0
			var s *Server
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet {
					writes++
					t.Error("unexpected write")
				}
				if tc.upstreamStatus != 0 {
					w.WriteHeader(tc.upstreamStatus)
					_, _ = io.WriteString(w, `{"error":"fixture failure"}`)
					return
				}
				primary := map[string]any{"id": "native-job", "role": "assistant", "triggeredByMessageId": "native-user", "status": tc.jobStatus, "inputNeeded": tc.inputNeeded}
				if tc.foreign {
					primary["triggeredByMessageId"] = "different-user"
				}
				messages := []any{map[string]string{"id": "native-user", "role": "user"}, primary}
				if tc.duplicate {
					messages = append(messages, primary)
				}
				if tc.bubble != "" {
					messages = append(messages, map[string]any{"id": "native-bubble", "role": "assistant", "triggeredByMessageId": "native-user", "source": "send_bubble", "presentation": map[string]string{"kind": "text"}, "status": "completed", "content": tc.bubble})
				}
				if tc.declinedNotice {
					messages = append(messages, map[string]any{"id": "declined", "role": "assistant", "source": "declined_notice", "presentation": map[string]string{"kind": "declined"}, "isStreaming": false, "content": tc.bubble})
				}
				if tc.mutate {
					s.projectMu.Lock()
					for _, p := range s.projects {
						p.Turns++
					}
					s.projectMu.Unlock()
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"messages": messages})
			}))
			defer upstream.Close()
			var dir string
			s, dir = resultFixture(t, upstream.URL, true)
			before, _ := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
			req := httptest.NewRequest(http.MethodGet, "/v1/sessions/original-session/result", nil)
			req = req.WithContext(WithEmbeddedAccount(req.Context(), SourceAccount{ID: "sub2api-50", Cookie: "original-cookie"}))
			rec := httptest.NewRecorder()
			s.EmbeddedHandler().ServeHTTP(rec, req)
			var got map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if tc.resultStatus != "" {
				if rec.Code != 200 || got["status"] != tc.resultStatus || got["error_code"] != tc.errorCode {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
				}
				result, _ := got["result"].(map[string]any)
				if result["text"] != tc.bubble {
					t.Fatalf("original body changed: %s", rec.Body)
				}
			} else if rec.Code < 400 || !bytes.Contains(rec.Body.Bytes(), []byte(tc.errorCode)) {
				t.Fatalf("unsafe read succeeded: %d %s", rec.Code, rec.Body)
			}
			after, _ := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
			if !bytes.Equal(before, after) || calls != 1 || writes != 0 {
				t.Fatal("lookup wrote state, retried or submitted a task")
			}
		})
	}
}

func TestSessionResultOriginalProxy(t *testing.T) {
	gets := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "original-upstream.invalid:80" {
			t.Error("original proxy CONNECT not used")
			w.WriteHeader(502)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		r, err = http.ReadRequest(rw.Reader)
		if err != nil {
			t.Error(err)
			return
		}
		gets++
		if r.Method != http.MethodGet || r.Host != "original-upstream.invalid" || r.URL.Query().Get("conversationId") != "original-project" || r.Header.Get("Cookie") != "original-cookie" || r.UserAgent() != "original-agent" {
			t.Error("original account route not preserved")
		}
		body := `{"messages":[{"id":"native-user","role":"user"},{"id":"native-job","role":"assistant","triggeredByMessageId":"native-user","status":"completed","content":"proxy result"}]}`
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		_ = rw.Flush()
	}))
	defer proxy.Close()
	s, _ := resultFixture(t, "http://original-upstream.invalid", true)
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/original-session/result", nil)
	req = req.WithContext(WithEmbeddedAccount(req.Context(), SourceAccount{ID: "sub2api-50", Cookie: "original-cookie", UserAgent: "original-agent", Proxy: proxy.URL}))
	rec := httptest.NewRecorder()
	s.EmbeddedHandler().ServeHTTP(rec, req)
	if rec.Code != 200 || gets != 1 || !bytes.Contains(rec.Body.Bytes(), []byte("proxy result")) {
		t.Fatalf("original proxy unused: %d %s calls=%d", rec.Code, rec.Body, gets)
	}
}
