package harkembed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed/gateway"
)

func TestHarkAcceptedPollFailurePreservesTurn(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, failureStatus := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusUnauthorized} {
				t.Run(fmt.Sprintf("%s/stream=%t/status=%d", protocol, stream, failureStatus), func(t *testing.T) {
					t.Setenv("HARK_DATA_DIR", t.TempDir())
					t.Setenv("HARK_USAGE_ESTIMATE", "off")
					var mu sync.Mutex
					creates, sends, archives := 0, 0, 0
					var sent map[string]string
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						defer mu.Unlock()
						w.Header().Set("Content-Type", "application/json")
						switch r.URL.Path {
						case "/api/conversations":
							creates++
							_ = json.NewEncoder(w).Encode(map[string]string{"conversationId": "original-account50-project"})
						case "/api/messages/send":
							sends++
							var payload map[string]json.RawMessage
							if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
								t.Error(err)
							}
							sent = make(map[string]string)
							for _, k := range []string{"idempotencyKey", "responseMessageId"} {
								var value string
								_ = json.Unmarshal(payload[k], &value)
								sent[k] = value
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": "original-account50-project", "messageId": sent["idempotencyKey"], "redirected": false})
						case "/api/messages/history":
							w.WriteHeader(failureStatus)
							_, _ = io.WriteString(w, `{"error":"rate limited while reading history"}`)
						default:
							if strings.Contains(r.URL.Path, "archive") {
								archives++
							}
							_, _ = io.WriteString(w, `{}`)
						}
					}))
					t.Cleanup(upstream.Close)
					previous, err := get()
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { server = previous })
					dataDir := t.TempDir()
					restart := func() {
						var err error
						server, err = gateway.NewEmbedded(dataDir, gateway.Options{Upstream: upstream.URL, PollInterval: time.Millisecond})
						if err != nil {
							t.Fatal(err)
						}
					}
					restart()
					path, body := identityForwardRequest(protocol, "Inspect existing task only", stream)
					var obj map[string]any
					_ = json.Unmarshal([]byte(body), &obj)
					obj["session_id"] = "accepted-session"
					b, _ := json.Marshal(obj)
					submit := func() (*http.Response, string) {
						req := labelRequest(t, path, string(b), "claude-opus-5-5")
						req.Header.Set("Authorization", "Bearer fixture-cookie")
						resp, err := Do(req, "", 50)
						if err != nil {
							t.Fatal(err)
						}
						data, err := io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						if err != nil {
							t.Fatal(err)
						}
						return resp, string(data)
					}
					resp, text := submit()
					if !strings.Contains(text, "upstream_turn_interrupted") {
						t.Fatalf("accepted history-read failure can still rotate accounts: http=%d body=%s", resp.StatusCode, text)
					}
					raw, err := os.ReadFile(filepath.Join(dataDir, "project-sessions.json"))
					if err != nil {
						t.Fatal(err)
					}
					var projects map[string]map[string]any
					_ = json.Unmarshal(raw, &projects)
					if len(projects) != 1 {
						t.Fatal("original project was not retained")
					}
					for _, p := range projects {
						pending, ok := p["pending_turn"].(map[string]any)
						if !ok || pending["request_id"] != sent["idempotencyKey"] || pending["expected"] != sent["responseMessageId"] || p["account_id"] != "sub2api-50" || p["turns"] != float64(1) {
							t.Fatal("accepted correlation IDs or original account/cursors were not persisted")
						}
					}
					restart()
					resp, text = submit()
					if resp.StatusCode != http.StatusConflict || !strings.Contains(text, "session_turn_pending") {
						t.Fatalf("restart/retry did not preserve the unresolved turn: http=%d body=%s", resp.StatusCode, text)
					}
					mu.Lock()
					defer mu.Unlock()
					if creates != 1 || sends != 1 || archives != 0 {
						t.Fatalf("accepted task replayed: creates=%d sends=%d archives=%d", creates, sends, archives)
					}
					t.Log("ACCEPTED_TURN preserved=true submissions=1 archives=0 restart_retry=pending")
				})
			}
		}
	}
}

func TestHarkEmptyProjectIsNotLegacyHistory(t *testing.T) {
	for _, originalOwner := range []bool{false, true} {
		t.Run(fmt.Sprintf("original_owner=%t", originalOwner), func(t *testing.T) {
			testHarkEmptyProject(t, originalOwner)
		})
	}
}

func testHarkEmptyProject(t *testing.T, originalOwner bool) {
	t.Helper()
	t.Setenv("HARK_DATA_DIR", t.TempDir())
	t.Setenv("HARK_USAGE_ESTIMATE", "off")
	dataDir := t.TempDir()
	sum := sha256.Sum256([]byte("embed:sub2api-39\x00empty-session"))
	key := hex.EncodeToString(sum[:])
	state := map[string]any{key: map[string]any{"mapping_id": key, "account_id": "sub2api-39", "conversation_id": "unsubmitted-project39", "last_used": "2026-11-15T13:38:21Z", "prompt_version": 2}}
	var ownerKey string
	var owner map[string]any
	if originalOwner {
		sum := sha256.Sum256([]byte("embed:sub2api-50\x00empty-session"))
		ownerKey = hex.EncodeToString(sum[:])
		owner = map[string]any{"mapping_id": ownerKey, "account_id": "sub2api-50", "conversation_id": "original-account50-project", "last_used": "2026-11-15T13:37:55Z", "prompt_version": 2, "turns": 3, "label_history_version": 2, "sent": []string{"legacy-cursor"}, "client_sent": []string{"accepted-cursor"}}
		state[ownerKey] = owner
	}
	raw, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dataDir, "project-sessions.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	creates, sends, archives := 0, 0, 0
	var sent map[string]string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/messages/send":
			sends++
			var payload map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&payload)
			sent = make(map[string]string)
			for _, k := range []string{"idempotencyKey", "responseMessageId"} {
				var value string
				_ = json.Unmarshal(payload[k], &value)
				sent[k] = value
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": "unsubmitted-project39", "messageId": sent["idempotencyKey"], "redirected": false})
		case "/api/messages/history":
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{"id": sent["responseMessageId"], "role": "assistant", "triggeredByMessageId": sent["idempotencyKey"], "jobStatus": "completed", "isStreaming": false, "content": "fixture complete"}}})
		case "/api/conversations":
			creates++
		default:
			if strings.Contains(r.URL.Path, "archive") {
				archives++
			}
		}
	}))
	t.Cleanup(upstream.Close)
	previous, err := get()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server = previous })
	server, err = gateway.NewEmbedded(dataDir, gateway.Options{Upstream: upstream.URL, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"hark","session_id":"empty-session","input":[{"role":"user","content":"Prior client context"},{"role":"assistant","content":"Prior client answer"},{"role":"user","content":"Current task"}]}`
	req := labelRequest(t, "/v1/responses", body, "claude-opus-5-5")
	req.Header.Set("Authorization", "Bearer fixture-cookie")
	resp, err := Do(req, "", 39)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if originalOwner {
		if resp.StatusCode != http.StatusConflict || !strings.Contains(string(data), "session_account_conflict") {
			t.Fatalf("empty project was allowed to replay another account's accepted context: http=%d body=%s", resp.StatusCode, data)
		}
		raw, err := os.ReadFile(filepath.Join(dataDir, "project-sessions.json"))
		if err != nil {
			t.Fatal(err)
		}
		var after map[string]json.RawMessage
		_ = json.Unmarshal(raw, &after)
		var got map[string]any
		_ = json.Unmarshal(after[ownerKey], &got)
		before, _ := json.Marshal(owner)
		unchanged, _ := json.Marshal(got)
		if string(before) != string(unchanged) {
			t.Fatal("original account50 Project/cursors changed")
		}
		mu.Lock()
		defer mu.Unlock()
		if sends != 0 || creates != 0 || archives != 0 {
			t.Fatal("another account's accepted turn was replayed or archived")
		}
		return
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), "fixture complete") {
		t.Fatalf("never-accepted empty project was treated as legacy history: http=%d body=%s", resp.StatusCode, data)
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 0 || sends != 1 || archives != 0 {
		t.Fatalf("empty project was replaced or archived: creates=%d sends=%d archives=%d", creates, sends, archives)
	}
}
