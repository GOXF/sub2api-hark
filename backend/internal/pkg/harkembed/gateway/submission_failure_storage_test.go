package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSubmissionFailureStorageAndLegacy(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		name := "rejection-evidence-save-fails"
		if accepted {
			name = "accepted-ack-save-fails"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			posts := 0
			var s *Server
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/conversations":
					_, _ = w.Write([]byte(`{"conversationId":"storage-original-project"}`))
				case "/api/messages/send":
					posts++
					var input map[string]any
					_ = json.NewDecoder(r.Body).Decode(&input)
					// Initial intent has already been written. Fail only the
					// subsequent acknowledgment/evidence save, not the POST.
					s.data = filepath.Join(dir, "missing-parent")
					if accepted {
						_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": "storage-original-project", "messageId": input["idempotencyKey"], "redirected": false})
					} else {
						w.WriteHeader(400)
						_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
					}
				default:
					_, _ = w.Write([]byte(`{}`))
				}
			}))
			defer up.Close()
			var err error
			s, err = NewEmbedded(dir, Options{Upstream: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			path, body := previewRequest("responses", false)
			post := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", path, strings.NewReader(body))
				r.Header.Set("X-Hark-Session-Id", "storage-session")
				r = r.WithContext(WithEmbeddedAccount(r.Context(), SourceAccount{ID: "fixture-account", Cookie: "fixture-cookie"}))
				w := httptest.NewRecorder()
				s.CallerHandler("fixture-owner").ServeHTTP(w, r)
				return w
			}
			first := post()
			cause := "upstream_request_rejected"
			if accepted {
				cause = "recovery_store_failed"
			}
			if first.Code != 503 || !strings.Contains(first.Body.String(), "recovery_error_record_failed") || !strings.Contains(first.Body.String(), cause) {
				t.Fatalf("save failure lost original cause: %d %s", first.Code, first.Body)
			}
			// The only successful durable write was the original intent.
			raw, _ := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
			var records map[string]map[string]any
			_ = json.Unmarshal(raw, &records)
			for _, row := range records {
				pending, _ := row["pending_turn"].(map[string]any)
				if pending == nil || pending["accepted"] == true || pending["submission_failure"] != nil {
					t.Fatal("failed save invented acknowledgment or evidence")
				}
			}
			s, err = NewEmbedded(dir, Options{Upstream: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			again := post()
			if again.Code != 409 || !strings.Contains(again.Body.String(), "session_turn_pending") || posts != 1 {
				t.Fatalf("restart cleared or replayed unknown intent: %d posts=%d %s", again.Code, posts, again.Body)
			}
		})
	}
	t.Run("tail-error-repeated-keeps-cause", func(t *testing.T) {
		x := issuedDurable(t)
		w := x.post("fixture-key-A", x.path, x.body, false)
		id := regexp.MustCompile(`call_[a-f0-9]+`).FindString(w.Body.String())
		body := durableTail("responses", x.body, id)
		x.tailFailure = 503
		w = x.post("fixture-key-A", x.path, body, false)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "upstream_http_error") {
			t.Fatal(w.Body)
		}
		x.restart()
		w = x.post("fixture-key-A", x.path, body, false)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "upstream_http_error") || x.sends != 2 {
			t.Fatal("old delivery guard masked tail error or replayed it")
		}
	})
	t.Run("old-record-stays-unknown", func(t *testing.T) {
		x := newDurableRig(t, "responses", false)
		x.post("fixture-key-A", x.path, x.body, false)
		for _, p := range x.s.projects {
			// Simulate a pre-candidate unconfirmed intent without evidence.
			p.PendingTurn.Accepted = false
			p.PendingTurn.Failure = nil
		}
		if err := x.s.persistProjects(); err != nil {
			t.Fatal(err)
		}
		x.restart()
		w := x.post("fixture-key-A", x.path, x.body, false)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "session_turn_pending") || x.sends != 1 {
			t.Fatal("legacy cause invented or old intent replayed")
		}
	})
}
