package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// This optional test hosts a loopback-only preview for an installed, unmodified
// Codex app-server. The Python driver supplies a fresh isolated CODEX_HOME and
// returns constant output for fixture_probe; no shell/file tool is executed.
// It seeds SYNTHETIC accepted metadata from the first request, not real history.
func TestRecoveryPreviewCodexLoopback(t *testing.T) {
	evidence := os.Getenv("HARK_CODEX_PREVIEW_DIR")
	if evidence == "" {
		t.Skip("explicit isolated Codex driver required")
	}
	if err := os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	s, dir, _, _, _, gets, writes := previewFixture(t, "responses", true, previewToolText)
	var mu sync.Mutex
	var preview http.Handler
	var recoveredCallID string
	var before []byte
	var records []map[string]any
	loopback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.Error(w, "fixture accepts Responses POST only", 404)
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, s.maxChatBody()+1))
		if err != nil {
			t.Error(err)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(b))
		// The experiment's explicit fixture binding, not a client modification.
		r.Header.Set("X-Hark-Session-Id", "original-session")
		r = r.WithContext(WithEmbeddedAccount(r.Context(), SourceAccount{ID: "sub2api-50", Cookie: "original-cookie"}))
		if preview == nil {
			v, err := s.normalizeResponses(b)
			if err != nil {
				t.Errorf("Codex request not accepted: %v", err)
				http.Error(w, "invalid fixture request", 400)
				return
			}
			toolSeen := false
			for _, tool := range v.Tools {
				toolSeen = toolSeen || tool.Name == "fixture_probe"
			}
			if !toolSeen {
				t.Error("Codex did not advertise dynamic fixture_probe")
				http.Error(w, "missing mock tool", 400)
				return
			}
			for _, p := range s.projects {
				p.ClientSent = hashes(v.Messages)
				p.ClientSystems = systemHashes(v.Messages, nil)
			}
			if err := s.persistProjects(); err != nil {
				t.Error(err)
				return
			}
			before, err = os.ReadFile(filepath.Join(dir, "project-sessions.json"))
			if err != nil {
				t.Error(err)
				return
			}
			proof := map[string]any{"account_id": "sub2api-50", "session_id": "original-session", "project_id": "original-project", "request_id": "native-user", "response_message_id": "native-job", "path": "/v1/responses", "request_sha256": previewSHA(b), "result_sha256": previewSHA([]byte(previewToolText))}
			preview = previewHandler(t, s, proof)
			if err := os.WriteFile(filepath.Join(evidence, "captured-client-request.private.json"), b, 0600); err != nil {
				t.Error(err)
				return
			}
		}
		rec := httptest.NewRecorder()
		preview.ServeHTTP(rec, r)
		if len(records) == 0 {
			ids := regexp.MustCompile(`call_[a-f0-9]+`).FindAllString(rec.Body.String(), -1)
			if len(ids) == 0 {
				t.Error("preview response missing stable call ID")
			} else {
				recoveredCallID = ids[0]
			}
		} else {
			v, err := s.normalizeResponses(b)
			outputBound := false
			if err == nil {
				for _, m := range v.Messages {
					outputBound = outputBound || m.Role == "tool" && strings.Contains(m.Content, "[call_id="+recoveredCallID+"]") && strings.Contains(m.Content, "MOCK_RESULT fixture-only")
				}
			}
			if !outputBound {
				t.Error("unmodified client did not echo mock result with recovered call ID")
			}
		}
		records = append(records, map[string]any{"body_sha256": previewSHA(b), "request_bytes": len(b), "status": rec.Code, "response_sha256": previewSHA(rec.Body.Bytes())})
		for k, values := range rec.Header() {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	defer loopback.Close()
	if err := os.WriteFile(filepath.Join(evidence, "server-url.txt"), []byte(loopback.URL), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(100 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(evidence, "driver-finished.json")); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(evidence, "driver-finished.json")); err != nil {
		t.Fatal("isolated Codex driver did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	after, _ := os.ReadFile(filepath.Join(dir, "project-sessions.json"))
	if len(before) == 0 || !bytes.Equal(before, after) || *gets != 1 || *writes != 0 {
		t.Fatalf("unsafe Codex preview: GETs=%d writes=%d records=%v", *gets, *writes, records)
	}
	if len(records) < 2 || records[0]["status"] != 200 || records[1]["status"] != 409 {
		t.Fatalf("expected recovered tool delivery then explicit continuation guard: %v", records)
	}
	result := map[string]any{"client": "installed unmodified Codex app-server", "synthetic_fixture_only": true, "requests": records, "original_GETs": *gets, "upstream_writes": *writes, "pending_and_context_unchanged": true, "continuation_acknowledged": false, "real_shell_or_file_tools_executed": false, "scope": "wire parsing + dynamic mock-tool round trip only; second request deliberately blocked, not a completed resumed task"}
	raw, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(filepath.Join(evidence, "GATEWAY_RESULT.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("CODEX_PREVIEW accepted_mock_tool=true next_request=409 original_GETs=1 writes=0 pending_preserved=true")
}
