package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInstalledCodexRelay(t *testing.T) {
	if os.Getenv("CODEX_ATTACHMENT_SMOKE") != "1" {
		t.Skip("set CODEX_ATTACHMENT_SMOKE=1 to exercise an isolated installed Codex runtime")
	}
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	root := testRoot(t)
	files := map[string][]byte{"marker.txt": []byte("ACTUAL-CODEX-TXT"), "marker.xls": {0xd0, 0xcf, 0x11, 0xe0, 0, 255}}
	for n, data := range files {
		if err := root.WriteFile(n, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	prompt := wrapper(filepath.Join(root.Name(), "marker.txt"), filepath.Join(root.Name(), "marker.xls"))
	var requests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"object":"list","data":[],"models":[]}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		wire, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		got := fileParts(t, wire)
		answer := "ONLY_LOCAL_PATHS"
		if len(got) != 0 {
			if len(got) != len(files) {
				t.Errorf("unexpected attachment count: %d", len(got))
			}
			for n, expected := range files {
				if !bytes.Equal(got[n], expected) {
					t.Errorf("%s bytes did not match", n)
				}
			}
			if bytes.Contains(wire, []byte(filepath.ToSlash(filepath.Join(root.Name(), "marker.txt")))) {
				t.Error("attachment path escaped relay")
			}
			answer = string(got["marker.txt"])
		}
		requests.Add(1)
		writeCodexStub(w, answer)
	}))
	defer backend.Close()
	target, _ := upstreamOrigin(backend.URL + "/v1")
	var confirmations atomic.Int32
	relay := httptest.NewServer(newRelay(root, target, testRelayToken, func(_ string, paths []string) bool {
		if len(paths) != len(files) {
			t.Errorf("expected %d files, got %d", len(files), len(paths))
		}
		confirmations.Add(1)
		return true // isolated fixture feedback only; production still uses the OS dialog
	}))
	defer relay.Close()

	t.Run("direct_baseline", func(t *testing.T) {
		before := requests.Load()
		got := runCodexSmoke(t, executable, backend.URL, "", []string{prompt})
		if got[0] != "ONLY_LOCAL_PATHS" || requests.Load()-before != 1 {
			t.Fatalf("baseline did not reproduce the missing attachment: %q", got)
		}
		t.Logf("BASELINE installed Codex direct: %s", got[0])
	})
	t.Run("relay_and_followup", func(t *testing.T) {
		before := requests.Load()
		got := runCodexSmoke(t, executable, relay.URL, testRelayToken, []string{prompt, "再原样输出上一轮的测试值。"})
		if got[0] != "ACTUAL-CODEX-TXT" || got[1] != "ACTUAL-CODEX-TXT" || confirmations.Load() != 1 || requests.Load()-before != 2 {
			t.Fatalf("candidate: answers=%q confirmations=%d", got, confirmations.Load())
		}
		t.Logf("MODIFIED installed Codex: initial=%s follow-up=%s confirmations=%d", got[0], got[1], confirmations.Load())
	})
}

func writeCodexStub(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	reply := map[string]any{"id": "resp_fixture", "object": "response", "created_at": time.Now().Unix(),
		"model": "gpt-5.4", "status": "in_progress", "output": []any{}, "usage": nil}
	message := map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant",
		"status": "in_progress", "content": []any{}, "phase": "final_answer"}
	event := func(typ string, p map[string]any) {
		p["type"] = typ
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, wrapJSON(p))
	}
	event("response.created", map[string]any{"response": reply})
	event("response.output_item.added", map[string]any{"output_index": 0, "item": message})
	event("response.output_text.delta", map[string]any{"output_index": 0, "item_id": "msg_fixture", "content_index": 0, "delta": text})
	message["status"] = "completed"
	message["content"] = []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}
	event("response.output_item.done", map[string]any{"output_index": 0, "item": message})
	reply["status"], reply["output"] = "completed", []any{message}
	reply["usage"] = map[string]int{"input_tokens": 4, "output_tokens": 5, "total_tokens": 9}
	event("response.completed", map[string]any{"response": reply})
}

func runCodexSmoke(t *testing.T, cli, target, localToken string, prompts []string) []string {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "codex-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	config := `model = "gpt-5.4"
model_provider = "fixture"
approval_policy = "never"
sandbox_mode = "read-only"
[analytics]
enabled = false
[model_providers.fixture]
name = "Loopback fixture"
base_url = "` + target + `/v1"
requires_openai_auth = false
supports_websockets = false
wire_api = "responses"
request_max_retries = 0
stream_max_retries = 0
`
	if localToken != "" {
		config += "[model_providers.fixture.http_headers]\n" + fmt.Sprintf("%q = %q\n", relayTokenHeader, localToken)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, cli, "app-server", "--stdio")
	c.Dir = dir
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(strings.ToUpper(kv), "=")
		if !strings.HasPrefix(name, "CODEX_") && !strings.HasPrefix(name, "OPENAI_") &&
			name != "HTTP_PROXY" && name != "HTTPS_PROXY" && name != "ALL_PROXY" && name != "NO_PROXY" && name != "OTEL_SDK_DISABLED" {
			c.Env = append(c.Env, kv)
		}
	}
	c.Env = append(c.Env, "CODEX_HOME="+home, "OTEL_SDK_DISABLED=true",
		"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "ALL_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1,localhost")
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "codex.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	c.Stderr = log
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = in.Close()
		_ = c.Process.Kill()
		_ = c.Wait()
	}()
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 65536), 2<<20)
	send := func(id int, method string, params any) {
		p := map[string]any{"jsonrpc": "2.0", "method": method}
		if params != nil {
			p["params"] = params
		}
		if id > 0 {
			p["id"] = id
		}
		if err := json.NewEncoder(in).Encode(p); err != nil {
			t.Fatal(err)
		}
	}
	read := func() object {
		for scanner.Scan() {
			var evt object
			if json.Unmarshal(scanner.Bytes(), &evt) == nil {
				return evt
			}
		}
		stderr, _ := os.ReadFile(log.Name())
		t.Fatalf("Codex stream ended: %v; %s", scanner.Err(), stderr)
		return nil
	}
	wait := func(id byte) json.RawMessage {
		for {
			evt := read()
			if bytes.Equal(evt["id"], []byte{id}) {
				if len(evt["error"]) != 0 {
					t.Fatalf("Codex RPC error: %s", evt["error"])
				}
				return evt["result"]
			}
		}
	}
	send(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "attachment-smoke", "title": "Attachment smoke", "version": "1"}})
	wait('1')
	send(0, "initialized", nil)
	send(2, "thread/start", map[string]any{"cwd": dir, "ephemeral": true})
	var started struct{ Thread struct{ ID string } }
	if err := json.Unmarshal(wait('2'), &started); err != nil || started.Thread.ID == "" {
		t.Fatalf("could not start thread: %+v %v", started, err)
	}
	var answers []string
	for i, prompt := range prompts {
		send(3+i, "turn/start", map[string]any{"threadId": started.Thread.ID, "input": []map[string]string{{"type": "text", "text": prompt}}})
		var answer strings.Builder
		for finished := false; !finished; {
			evt := read()
			if len(evt["error"]) != 0 {
				t.Fatalf("turn RPC error: %s", evt["error"])
			}
			var params struct {
				Delta string
				Turn  struct {
					Status string
					Error  json.RawMessage
				}
			}
			_ = json.Unmarshal(evt["params"], &params)
			switch asString(evt["method"]) {
			case "item/agentMessage/delta":
				_, _ = answer.WriteString(params.Delta)
			case "turn/completed":
				if params.Turn.Status != "completed" {
					t.Fatalf("turn %d %s: %s", i+1, params.Turn.Status, params.Turn.Error)
				}
				finished = true
			}
		}
		answers = append(answers, answer.String())
	}
	return answers
}
