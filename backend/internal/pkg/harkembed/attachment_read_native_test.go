package harkembed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed/gateway"
)

// Opt-in: unchanged Codex -> actual host intake/Do/usage wrappers -> mock Hark.
func TestAttachmentNativeCodex(t *testing.T) {
	evidence := os.Getenv("HARK_CODEX_ATTACHMENT_DIR")
	if evidence == "" {
		t.Skip("explicit native Codex synthetic acceptance required")
	}
	fixture := os.Getenv("HARK_ATTACHMENT_FIXTURE")
	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var data []byte
	var sent map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/conversations":
			io.WriteString(w, `{"conversationId":"native-fixture"}`)
		case "/api/files/upload/native-fixture/presign":
			json.NewEncoder(w).Encode(map[string]string{"upload_url": "http://" + r.Host + "/upload", "attachment_id": "native-file", "path": "native-path", "s3_key": "native-key"})
		case "/upload":
			data, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{}`)
		case "/api/files/native-fixture/native-file/finalize":
			io.WriteString(w, `{}`)
		case "/api/messages/send":
			json.NewDecoder(r.Body).Decode(&sent)
			var id string
			json.Unmarshal(sent["idempotencyKey"], &id)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": "native-fixture", "messageId": id, "redirected": false})
		case "/api/messages/history":
			var id, responseID string
			json.Unmarshal(sent["idempotencyKey"], &id)
			json.Unmarshal(sent["responseMessageId"], &responseID)
			marker := regexp.MustCompile(`ATTACHMENT_HIDDEN_[a-zA-Z0-9]+`).Find(data)
			json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{"id": responseID, "role": "assistant", "triggeredByMessageId": id, "jobStatus": "completed", "isStreaming": false, "content": string(marker)}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	upstreamURL, proxyURL, apiKey := upstream.URL, "", "synthetic=fixture"
	live := os.Getenv("HARK_ATTACHMENT_ACCOUNT_HELPER") != ""
	if live {
		// Read credentials through a private process pipe; never write or log them.
		secret, err := exec.Command("python", "-X", "utf8", os.Getenv("HARK_ATTACHMENT_ACCOUNT_HELPER")).Output()
		if err != nil {
			t.Fatal("private live account helper failed")
		}
		var account struct {
			APIKey string `json:"api_key"`
			Proxy  string `json:"proxy"`
		}
		if json.Unmarshal(secret, &account) != nil || account.APIKey == "" {
			t.Fatal("live account credentials unavailable")
		}
		upstreamURL, proxyURL, apiKey = "https://hark.com", account.Proxy, account.APIKey
	}
	dataDir := t.TempDir()
	if live {
		dataDir = filepath.Join(evidence, "gateway-data")
	}
	t.Setenv("HARK_DATA_DIR", dataDir)
	s, err := get()
	if err != nil {
		t.Fatal(err)
	}
	pollInterval := time.Millisecond
	if live {
		pollInterval = time.Second
	}
	if err = s.Configure(gateway.Options{Upstream: upstreamURL, PollInterval: pollInterval}); err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	var callback []byte
	var endpoint string
	loopback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			b, _ := io.ReadAll(io.LimitReader(r.Body, 21<<20))
			r.Body = io.NopCloser(bytes.NewReader(b))
			mu.Lock()
			callback = append([]byte(nil), b...)
			mu.Unlock()
			rec := httptest.NewRecorder()
			AttachmentUploadHandler().ServeHTTP(rec, r)
			if os.Getenv("HARK_ATTACHMENT_SLOW_RECEIPT") == "true" {
				time.Sleep(33 * time.Second)
			}
			for k, values := range rec.Header() {
				w.Header()[k] = values
			}
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes())
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 83<<20))
		r.Body = io.NopCloser(bytes.NewReader(b))
		var body any
		json.Unmarshal(b, &body)
		mu.Lock()
		records = append(records, map[string]any{"path": r.URL.Path, "body": body})
		mu.Unlock()
		r = r.WithContext(context.WithValue(r.Context(), ctxkey.APIKeyID, int64(424242)))
		if PreflightAttachmentRead(w, r, endpoint) {
			return
		}
		r.Header.Set("Authorization", "Bearer "+apiKey)
		r.URL.Scheme, r.URL.Host = "http", Host
		resp, err := Do(r, proxyURL, 424242)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		mu.Lock()
		records[len(records)-1]["response_headers"] = resp.Header.Clone()
		mu.Unlock()
		for k, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(k, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		io.Copy(w, resp.Body)
	}))
	defer loopback.Close()
	endpoint = loopback.URL
	_, source, _, _ := runtime.Caller(0)
	driver := filepath.Join(filepath.Dir(source), "gateway", "testdata", "attachment_native_client.py")
	cmd := exec.Command("python", "-X", "utf8", driver, endpoint, fixture, evidence)
	output, err := cmd.CombinedOutput()
	os.WriteFile(filepath.Join(evidence, "native-driver.stdout-stderr.txt"), output, 0600)
	mu.Lock()
	b, _ := json.MarshalIndent(records, "", "  ")
	got := append([]byte(nil), callback...)
	storage := append([]byte(nil), data...)
	mu.Unlock()
	os.WriteFile(filepath.Join(evidence, "gateway-requests.json"), b, 0600)
	if err != nil {
		t.Fatalf("native client: %v: %s", err, output)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("uploaded bytes %d != fixture %d", len(got), len(want))
	}
	if !live && !bytes.Equal(storage, want) {
		t.Fatal("mock Hark storage did not receive exact bytes")
	}
	marker := regexp.MustCompile(`ATTACHMENT_HIDDEN_[a-zA-Z0-9]+`).Find(got)
	final, err := os.ReadFile(filepath.Join(evidence, "final.txt"))
	if err != nil || len(marker) == 0 || !bytes.Contains(final, marker) {
		t.Fatalf("no uploaded marker in answer: %q, %v", final, err)
	}
	expectedRequests := 2
	if os.Getenv("HARK_ATTACHMENT_SLOW_RECEIPT") == "true" {
		expectedRequests = 3
	}
	if len(records) != expectedRequests {
		t.Fatalf("expected %d automatic requests, got %d", expectedRequests, len(records))
	}
	if strings.Contains(string(b), `"previous_response_id"`) {
		t.Fatal("unexpected previous_response_id")
	}
	sum := sha256.Sum256(got)
	upstreamProof := "mock Hark; answer extracted from uploaded bytes only"
	if live {
		upstreamProof = "real Hark; hidden marker was not supplied in the request"
	}
	result := map[string]any{"native_client": "unchanged installed Codex", "gateway": "actual product host preflight, Do, upload and usage wrappers", "upstream": upstreamProof, "bytes": len(got), "sha256": hex.EncodeToString(sum[:]), "requests": len(records), "hidden_marker_returned": true}
	b, _ = json.MarshalIndent(result, "", "  ")
	os.WriteFile(filepath.Join(evidence, "RESULT.json"), b, 0600)
	t.Log(string(b))
}
