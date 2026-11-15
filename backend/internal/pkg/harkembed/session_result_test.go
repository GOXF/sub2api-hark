package harkembed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed/gateway"
)

func TestHarkSessionResultReadOnly(t *testing.T) {
	const original = "Hark original result: <tool_call>{\"name\":\"exec_command\",\"arguments\":{}}</tool_call>"
	writes, gets := 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/messages/history" {
			writes++
			t.Error("unexpected upstream operation")
			return
		}
		gets++
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{
			map[string]string{"id": "native-request", "role": "user"},
			map[string]string{"id": "native-response", "role": "assistant", "triggeredByMessageId": "native-request", "status": "completed", "content": original},
		}})
	}))
	defer upstream.Close()
	t.Setenv("HARK_DATA_DIR", t.TempDir())
	previous, err := get()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server = previous })
	dir := t.TempDir()
	key := sha256.Sum256([]byte("embed:sub2api-50\x00embedded-result-test"))
	state, _ := json.Marshal(map[string]any{hex.EncodeToString(key[:]): map[string]any{
		"mapping_id": hex.EncodeToString(key[:]), "account_id": "sub2api-50", "conversation_id": "original-project", "turns": 3, "last_used": time.Now().UTC(),
		"pending_turn": map[string]string{"request_id": "native-request", "expected": "native-response"},
	}})
	store := filepath.Join(dir, "project-sessions.json")
	if err := os.WriteFile(store, state, 0600); err != nil {
		t.Fatal(err)
	}
	server, err = gateway.NewEmbedded(dir, gateway.Options{Upstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, BaseURL+"/v1/sessions/embedded-result-test/result", nil)
	req.Header.Set("Authorization", "Bearer fixture-cookie")
	ctx := context.WithValue(req.Context(), ctxkey.HarkRequestedModel, "claude-mapped-fixture")
	resp, err := Do(req.WithContext(ctx), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ReadOnly  bool   `json:"read_only"`
		Continued bool   `json:"continuation_applied"`
		Status    string `json:"status"`
		Result    struct {
			Text string `json:"text"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !got.ReadOnly || got.Continued || got.Status != "awaiting_client_tool" || got.Result.Text != original {
		t.Fatalf("GET adapted or executed native diagnostic: %d %s", resp.StatusCode, body)
	}
	after, _ := os.ReadFile(store)
	if string(after) != string(state) || gets != 1 || writes != 0 {
		t.Fatal("lookup modified original context or replayed a task")
	}
	t.Log("EMBEDDED_RESULT original_text_preserved=true upstream_GET=1 upstream_write=0 context_unchanged=true")
}
