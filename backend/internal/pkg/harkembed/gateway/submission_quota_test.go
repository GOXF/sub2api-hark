package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSubmissionQuota402Evidence(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		rejected   bool
	}{
		{"known-native-code", `{"code":"token_limit_reached","error":"quota exhausted"}`, true},
		{"no-code", `{"error":"quota exhausted"}`, false},
		{"unknown-code", `{"code":"payment_required","error":"quota exhausted"}`, false},
		{"nested-code-only", `{"error":{"code":"token_limit_reached","message":"quota exhausted"}}`, false},
		{"empty-error", `{"code":"token_limit_reached","error":" "}`, false},
		{"null-error", `{"code":"token_limit_reached","error":null}`, false},
		{"wrong-error-shape", `{"code":"token_limit_reached","error":{"message":"quota exhausted"}}`, false},
		{"contradictory-success", `{"code":"token_limit_reached","error":"quota exhausted","success":true}`, false},
		{"contradictory-accepted", `{"code":"token_limit_reached","error":"quota exhausted","accepted":true}`, false},
		{"contradictory-redirected", `{"code":"token_limit_reached","error":"quota exhausted","redirected":true}`, false},
		{"malformed", `{"code":"token_limit_reached","error":"quota exhausted"`, false},
		{"html", `<html>token_limit_reached</html>`, false},
		{"oversized", `{"code":"token_limit_reached","error":"` + strings.Repeat("x", 65536) + `"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := explicitSubmissionRejection(402, []byte(tc.body)); got != tc.rejected {
				t.Fatalf("quota rejection proof: got=%t want=%t", got, tc.rejected)
			}
		})
	}
}

func TestSubmissionQuota402PartialResponse(t *testing.T) {
	body := `{"code":"token_limit_reached","error":"quota exhausted"}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)+10))
		w.WriteHeader(402)
		_, _ = w.Write([]byte(body))
	}))
	defer up.Close()
	s, err := NewEmbedded(t.TempDir(), Options{Upstream: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	err = s.harkJSON(context.Background(), "POST", "/api/messages/send", "fixture-cookie", map[string]string{"message": "fixture"}, nil)
	var native *nativeSubmissionError
	if !errors.As(err, &native) || native.failure.Outcome != "uncertain" || native.failure.Phase != "read_response" || native.failure.Code != "upstream_http_error" || native.failure.NativeStatus != 402 || native.failure.TransportError == "" {
		t.Fatalf("partial quota response became a full rejection: %v", err)
	}
}

func TestSubmissionQuota402LegacyPendingPreserved(t *testing.T) {
	quotaBody, err := os.ReadFile("testdata/submission_token_limit_402.json")
	if err != nil {
		t.Fatal(err)
	}
	posts, history := 0, 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/conversations":
			_, _ = w.Write([]byte(`{"conversationId":"quota-original-project"}`))
		case "/api/messages/send":
			posts++
			w.WriteHeader(402)
			_, _ = w.Write(quotaBody)
		case "/api/messages/history":
			history++
			_, _ = w.Write([]byte(`{"messages":[]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer up.Close()
	dir := t.TempDir()
	newServer := func() *Server {
		s, err := NewEmbedded(dir, Options{Upstream: up.URL})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	path, body := previewRequest("responses", false)
	request := func(s *Server, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Hark-Session-Id", "quota-legacy-session")
		r = r.WithContext(WithEmbeddedAccount(r.Context(), SourceAccount{ID: "fixture-account", Cookie: "fixture-cookie"}))
		w := httptest.NewRecorder()
		s.CallerHandler("fixture-owner").ServeHTTP(w, r)
		return w
	}
	request(newServer(), "POST", path, body)
	file := filepath.Join(dir, "project-sessions.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var rows map[string]map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != 1 {
		t.Fatalf("expected one fixture session: %s %v", raw, err)
	}
	for _, row := range rows {
		pending := row["pending_turn"].(map[string]any)
		failure := pending["submission_failure"].(map[string]any)
		// Model a pre-fix saved402. Loading new code must not upgrade it in place.
		failure["outcome"], failure["code"], failure["status"] = "uncertain", "upstream_http_error", 502
		failure["message"] = "Hark 返回 HTTP 402"
	}
	before, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, before, 0600); err != nil {
		t.Fatal(err)
	}
	s := newServer()
	diagnostic := request(s, "GET", "/v1/sessions/quota-legacy-session/result", "")
	if diagnostic.Code != 200 || !strings.Contains(diagnostic.Body.String(), `"submission_uncertain"`) {
		t.Fatalf("saved402 was reclassified: %d %s", diagnostic.Code, diagnostic.Body)
	}
	readOnly, err := os.ReadFile(file)
	if err != nil || string(readOnly) != string(before) {
		t.Fatal("read-only diagnosis changed stored pending bytes")
	}
	again := request(s, "POST", path, body)
	if again.Code != 409 || !strings.Contains(again.Body.String(), "upstream_http_error") {
		t.Fatalf("old pending was silently repaired or retried: %d %s", again.Code, again.Body)
	}
	after, err := os.ReadFile(file)
	if err != nil || posts != 1 || history != 0 {
		t.Fatalf("old pending changed/replayed: posts=%d history=%d read_error=%v", posts, history, err)
	}
	var actual map[string]map[string]any
	if err := json.Unmarshal(after, &actual); err != nil {
		t.Fatal(err)
	}
	// A denied POST still refreshes last_used; pending, identity and cursors must not change.
	for id, row := range rows {
		delete(row, "last_used")
		delete(actual[id], "last_used")
	}
	expected, _ := json.Marshal(rows)
	got, _ := json.Marshal(actual)
	if string(got) != string(expected) {
		t.Fatalf("legacy pending/context changed: before=%s after=%s", expected, got)
	}
	t.Log("LEGACY_QUOTA_PENDING context_unchanged=true readonly_bytes_identical=true native_POSTs=1 history_GETs=0 old_outcome=uncertain outward=409")
}
