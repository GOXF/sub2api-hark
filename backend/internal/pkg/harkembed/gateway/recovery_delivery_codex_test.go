package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Actual installed Codex, temporary profile, synthetic native gateway and one
// constant dynamic mock tool. No real old thread, shell or business tool is used.
func TestDurableRecoveryCodexLoopback(t *testing.T) {
	evidence := os.Getenv("HARK_CODEX_DURABLE_DIR")
	if evidence == "" {
		t.Skip("explicit isolated Codex driver required")
	}
	if err := os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	x := newDurableRig(t, "responses", true)
	var mu sync.Mutex
	var records []map[string]any
	loopback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.Error(w, "fixture Responses only", 404)
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, x.s.maxChatBody()+1))
		if err != nil {
			t.Error(err)
			return
		}
		_ = r.Body.Close()
		r.Header.Set("X-Hark-Session-Id", "original-session")
		if len(records) == 0 {
			x.body = string(b)
			// Capture a genuine synthetic acceptance/poll interruption, then
			// explicitly prepare the recovery BEFORE returning to this test client.
			v, err := x.s.normalizeResponses(b)
			if err != nil {
				t.Error(err)
				http.Error(w, "bad fixture request", 400)
				return
			}
			if len(v.Tools) == 0 {
				t.Error("missing client tool contract")
				return
			}
			initial := x.post("fixture-key-A", x.path, x.body, false)
			// Initial request needs the same fixture session header as the real client.
			if !strings.Contains(initial.Body.String(), "upstream_turn_interrupted") {
				t.Errorf("fixture did not retain accepted turn: %s", initial.Body)
				http.Error(w, "interruption fixture failed", 500)
				return
			}
			if prepared := x.prepare(); prepared.Code != 200 {
				t.Errorf("explicit synthetic preparation failed: %s", prepared.Body)
				http.Error(w, "prepare failed", 500)
				return
			}
			x.restart()
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		r = r.WithContext(WithEmbeddedAccount(r.Context(), SourceAccount{ID: "sub2api-50", Cookie: "fixture-cookie"}))
		rec := httptest.NewRecorder()
		durableCaller(t, x.s, "fixture-key-A").ServeHTTP(rec, r)
		records = append(records, map[string]any{"status": rec.Code, "request_bytes": len(b), "request_sha256": previewSHA(b), "response_sha256": previewSHA(rec.Body.Bytes())})
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
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(records) != 2 || records[0]["status"] != 200 || records[1]["status"] != 200 || x.sends != 2 || x.archives != 0 {
		t.Fatalf("durable client loop incomplete: records=%v sends=%d archives=%d", records, x.sends, x.archives)
	}
	tail, _ := x.turns[1]["message"].(string)
	if !strings.Contains(tail, "MOCK_RESULT fixture-only") || strings.Contains(tail, "Read the synthetic fixture only.") {
		t.Fatal("client tool output did not continue as a new tail only")
	}
	result := map[string]any{"client": "installed unmodified Codex app-server", "synthetic_fixture_only": true, "requests": records, "original_native_submissions": 1, "new_tool_output_submissions": 1, "archives": 0, "only_verified_tail_forwarded": true, "real_tools_executed": false, "scope": "explicit admin prepare interposed after synthetic acceptance failure; not import/replay of a real failed client thread"}
	raw, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(filepath.Join(evidence, "GATEWAY_RESULT.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("DURABLE_CODEX completed_mock_roundtrip=true original=1 tool_tail=1 archives=0 real_tools=0")
}
