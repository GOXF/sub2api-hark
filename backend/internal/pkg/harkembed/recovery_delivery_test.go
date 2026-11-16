package harkembed

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed/gateway"
)

func TestHarkDurableCallerBinding(t *testing.T) {
	t.Setenv("HARK_DATA_DIR", t.TempDir())
	previous, err := get()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server = previous })
	posts := 0
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/conversations":
			_, _ = io.WriteString(w, `{"conversationId":"original-owner-project"}`)
		case "/api/messages/send":
			posts++
			var p map[string]string
			_ = json.NewDecoder(r.Body).Decode(&p)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": "original-owner-project", "messageId": p["idempotencyKey"], "redirected": false})
		case "/api/messages/history":
			w.WriteHeader(429)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer u.Close()
	dir := t.TempDir()
	server, err = gateway.NewEmbedded(dir, gateway.Options{Upstream: u.URL})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"hark","session_id":"host-owner-test","input":"Inspect fixture only"}`
	submit := func(keyID int64) (*http.Response, []byte) {
		req := httptest.NewRequest(http.MethodPost, BaseURL+"/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer fixture-cookie")
		req.Header.Set("X-Recovery-Owner", "sub2api-key-11") // Untrusted header is irrelevant.
		req = req.WithContext(context.WithValue(req.Context(), ctxkey.APIKeyID, keyID))
		resp, err := Do(req, "", 50)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return resp, b
	}
	resp, text := submit(11)
	if resp.StatusCode != 409 || !strings.Contains(string(text), "upstream_turn_interrupted") {
		t.Fatalf("missing accepted error: %s", text)
	}
	path := filepath.Join(dir, "project-sessions.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var sessions map[string]map[string]any
	_ = json.Unmarshal(before, &sessions)
	for _, p := range sessions {
		if p["owner_id"] != "sub2api-key-11" {
			t.Fatal("authenticated key ID not bound")
		}
		pending, _ := p["pending_turn"].(map[string]any)
		if pending["contract"] == nil || pending["accepted"] != true {
			t.Fatal("original contract not persisted")
		}
	}
	resp, text = submit(12)
	if resp.StatusCode != 403 || !strings.Contains(string(text), "session_owner_mismatch") {
		t.Fatal("spoofed header overrode authenticated key")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || posts != 1 {
		t.Fatal("foreign caller modified or replayed original")
	}
	t.Log("HOST_CALLER authenticated_key_id=11 foreign_key=403 spoofed_header_ignored=true native_submissions=1")
}
