package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This slice deliberately retains pending. It must not silently turn a
// classification/provenance fix into permission to retry an original task.
func TestSubmissionFailureClassification(t *testing.T) {
	quotaBody, err := os.ReadFile("testdata/submission_token_limit_402.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name, body, outcome, cause string
				native, status             int
			}{
				{"explicit400", `{"error":"invalid_request","message":"fixture detail"}`, "rejected", "upstream_request_rejected", 400, 400},
				{"explicit401", `{"error":"unauthorized"}`, "rejected", "upstream_session_expired", 401, 502},
				{"explicit429", `{"error":"rate_limited"}`, "rejected", "upstream_rate_limited", 429, 429},
				{"nativeMonthlyQuota402", string(quotaBody), "rejected", "upstream_quota_exhausted", 402, 402},
				{"unknown402", `{"error":"Payment required","code":"unknown"}`, "uncertain", "upstream_http_error", 402, 409},
				{"quotaWordsOnly402", `{"error":"monthly quota exhausted"}`, "uncertain", "upstream_http_error", 402, 409},
				{"proxy402", `<html>Payment required</html>`, "uncertain", "upstream_http_error", 402, 409},
				{"contradictoryQuota402", `{"error":"quota exhausted","code":"token_limit_reached","accepted":true}`, "uncertain", "upstream_http_error", 402, 409},
				{"ambiguous503", `{"error":"synthetic response lost after possible acceptance"}`, "uncertain", "upstream_http_error", 503, 409},
				{"invalid200", `not-json`, "uncertain", "upstream_response_invalid", 200, 409},
				{"wrongAck200", `{"success":true,"conversationId":"another-project","messageId":"other","redirected":false}`, "uncertain", "upstream_project_mismatch", 200, 409},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", protocol, stream, tc.name), func(t *testing.T) {
					posts, history := 0, 0
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						switch r.URL.Path {
						case "/api/conversations":
							_, _ = w.Write([]byte(`{"conversationId":"submission-original-project"}`))
						case "/api/messages/send":
							posts++
							w.WriteHeader(tc.native)
							_, _ = w.Write([]byte(tc.body))
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
					s := newServer()
					path, body := previewRequest(protocol, stream)
					request := func(s *Server, method, path, body string) *httptest.ResponseRecorder {
						r := httptest.NewRequest(method, path, strings.NewReader(body))
						r.Header.Set("X-Hark-Session-Id", "submission-session")
						r = r.WithContext(WithEmbeddedAccount(r.Context(), SourceAccount{ID: "fixture-account", Cookie: "fixture-cookie"}))
						w := httptest.NewRecorder()
						s.CallerHandler("fixture-owner").ServeHTTP(w, r)
						return w
					}
					first := request(s, "POST", path, body)
					if first.Code != tc.status {
						t.Fatalf("classification HTTP mismatch: want=%d actual=%d %s", tc.status, first.Code, first.Body)
					}
					file := filepath.Join(dir, "project-sessions.json")
					raw, err := os.ReadFile(file)
					if err != nil {
						t.Fatal(err)
					}
					var rows map[string]map[string]any
					if err := json.Unmarshal(raw, &rows); err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						pending, ok := row["pending_turn"].(map[string]any)
						if !ok || pending["accepted"] == true {
							t.Fatal("classification cleared pending or invented acceptance")
						}
						failure, ok := pending["submission_failure"].(map[string]any)
						if !ok || failure["outcome"] != tc.outcome || failure["code"] != tc.cause || failure["native_http_status"] != float64(tc.native) {
							t.Fatalf("original cause/classification missing: %s", raw)
						}
						if failure["response_prefix_sha256"] != previewSHA([]byte(tc.body)) || failure["response_excerpt"] != tc.body {
							t.Fatal("original response provenance was lost")
						}
						if !strings.Contains(first.Body.String(), tc.cause) {
							t.Fatal("first client error masked original cause code")
						}
						if row["conversation_id"] != "submission-original-project" || row["owner_id"] != "fixture-owner" {
							t.Fatal("classification changed original association")
						}
					}
					s = newServer()
					// An administrator can read saved failure evidence without a
					// native history request or any state transition.
					diagnostic := request(s, "GET", "/v1/sessions/submission-session/result", "")
					if diagnostic.Code != 200 || !strings.Contains(diagnostic.Body.String(), `"submission_failure"`) || !strings.Contains(diagnostic.Body.String(), tc.cause) {
						t.Fatalf("saved failure diagnosis unavailable: %d %s", diagnostic.Code, diagnostic.Body)
					}
					after, _ := os.ReadFile(file)
					if string(raw) != string(after) {
						t.Fatal("read-only diagnosis changed pending/context bytes")
					}
					again := request(s, "POST", path, body)
					if again.Code != tc.status || !strings.Contains(again.Body.String(), tc.cause) {
						t.Fatalf("restart lost original failure: %d %s", again.Code, again.Body)
					}
					if posts != 1 || history != 0 {
						t.Fatalf("classification/receipt read replayed or contacted native history: posts=%d history=%d", posts, history)
					}
					t.Logf("SUBMISSION_PROVENANCE outcome=%s cause=%s native=%d outward=%d posts=1 pending_kept=true restart=true read_only_native_GETs=0", tc.outcome, tc.cause, tc.native, tc.status)
				})
			}
		}
	}
}
