package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type attachmentReadRig struct {
	s     *Server
	mu    sync.Mutex
	data  []byte
	sent  map[string]json.RawMessage
	calls int
}

func newAttachmentReadRig(t *testing.T) *attachmentReadRig {
	t.Helper()
	x := &attachmentReadRig{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.mu.Lock()
		defer x.mu.Unlock()
		x.calls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/conversations":
			io.WriteString(w, `{"conversationId":"read-fixture"}`)
		case "/api/files/upload/read-fixture/presign":
			json.NewEncoder(w).Encode(map[string]string{"upload_url": "http://" + r.Host + "/upload", "attachment_id": "read-file", "path": "read-path", "s3_key": "read-key"})
		case "/upload":
			x.data, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{}`)
		case "/api/files/read-fixture/read-file/finalize":
			io.WriteString(w, `{}`)
		case "/api/messages/send":
			json.NewDecoder(r.Body).Decode(&x.sent)
			var id string
			json.Unmarshal(x.sent["idempotencyKey"], &id)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": "read-fixture", "messageId": id, "redirected": false})
		case "/api/messages/history":
			var id, responseID string
			json.Unmarshal(x.sent["idempotencyKey"], &id)
			json.Unmarshal(x.sent["responseMessageId"], &responseID)
			// The answer is extracted only from bytes that reached the upload endpoint.
			marker := regexp.MustCompile(`ATTACHMENT_HIDDEN_[a-zA-Z0-9]+`).Find(x.data)
			text := "ordinary fixture reply"
			if len(marker) > 0 {
				text = string(marker)
			}
			json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{"id": responseID, "role": "assistant", "triggeredByMessageId": id, "jobStatus": "completed", "isStreaming": false, "content": text}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	var err error
	x.s, err = NewEmbedded(t.TempDir(), Options{Upstream: up.URL, PublicBase: "https://trusted.example", PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func attachmentReadBody(file string) map[string]any {
	return map[string]any{"model": "hark", "stream": false, "session_id": "read-session", "input": []any{
		map[string]any{"role": "user", "content": "# Files mentioned by the user:\n\n## selected: " + file + "\n\n## My request:\nRead the hidden marker from the attachment."},
	}, "tools": []any{map[string]any{"type": "function", "name": "exec_command", "parameters": json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"},"login":{"type":"boolean"},"max_output_tokens":{"type":"integer"}},"required":["cmd"],"additionalProperties":false}`)}}}
}

func (x *attachmentReadRig) post(t *testing.T, owner string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(WithEmbeddedAccount(context.Background(), SourceAccount{ID: "fixture-account", Cookie: "fixture-cookie"}))
	w := httptest.NewRecorder()
	x.s.CallerHandler(owner).ServeHTTP(w, r)
	return w
}

func attachmentReadCall(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Output []map[string]any `json:"output"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &env) != nil || len(env.Output) != 1 || env.Output[0]["type"] != "function_call" || env.Output[0]["name"] != "exec_command" {
		t.Fatalf("expected native attachment read call, HTTP %d: %s", w.Code, w.Body)
	}
	return env.Output[0]
}

func TestAttachmentReadCycle(t *testing.T) {
	for _, ext := range []string{"xls", "xlsx", "pdf", "png", "future"} {
		t.Run(ext, func(t *testing.T) {
			x := newAttachmentReadRig(t)
			body := attachmentReadBody("C:/selected/file." + ext)
			w := x.post(t, "caller-A", body)
			call := attachmentReadCall(t, w)
			if x.calls != 0 {
				t.Fatal("intake contacted upstream before file read")
			}
			var args struct {
				Cmd string `json:"cmd"`
			}
			json.Unmarshal([]byte(call["arguments"].(string)), &args)
			match := regexp.MustCompile(`PutAsync\('([^']+)'`).FindStringSubmatch(args.Cmd)
			if len(match) != 2 || !strings.HasPrefix(match[1], "https://trusted.example/v1/attachment-reads/") {
				t.Fatalf("untrusted upload command: %s", args.Cmd)
			}
			want := []byte("\x00ATTACHMENT_HIDDEN_" + ext + "\xff")
			r := httptest.NewRequest("PUT", match[1], bytes.NewReader(want))
			r.Header.Set("Content-Type", "application/octet-stream")
			upload := httptest.NewRecorder()
			x.s.Handler().ServeHTTP(upload, r)
			if upload.Code != 200 {
				t.Fatalf("upload HTTP %d: %s", upload.Code, upload.Body)
			}
			body["input"] = append(body["input"].([]any), call, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "Chunk ID: fixture\nProcess exited with code 0\nOutput:\n" + upload.Body.String()})
			w = x.post(t, "caller-A", body)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "ATTACHMENT_HIDDEN_"+ext) || !bytes.Equal(x.data, want) {
				t.Fatalf("closed loop HTTP %d: %s; uploaded %q", w.Code, w.Body, x.data)
			}
			var sent string
			json.Unmarshal(x.sent["message"], &sent)
			for _, forbidden := range []string{"C:/selected", "attachment-reads", "call_id=", "tool_calls:", "TOOLS (run"} {
				if strings.Contains(sent, forbidden) {
					t.Errorf("upstream leaked %s: %s", forbidden, sent)
				}
			}
			body["input"] = append(body["input"].([]any), map[string]any{"role": "assistant", "content": "seen"}, map[string]any{"role": "user", "content": "What did the attachment say?"})
			w = x.post(t, "caller-A", body)
			if w.Code != 200 || w.Header().Get("X-Hark-Project-Reused") != "true" || w.Header().Get("X-Hark-Uploaded-Files") != "0" {
				t.Fatalf("history reset/reuploaded: HTTP %d headers=%v body=%s", w.Code, w.Header(), w.Body)
			}
			fmt.Printf("READ_CYCLE %s: native-call -> exact %d bytes -> answer -> history retained\n", ext, len(want))
		})
	}
}
