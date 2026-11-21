package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func issuedAttachment(t *testing.T, x *attachmentReadRig, body map[string]any) (map[string]any, string) {
	t.Helper()
	call := attachmentReadCall(t, x.post(t, "caller-A", body))
	var args struct {
		Cmd string `json:"cmd"`
	}
	json.Unmarshal([]byte(call["arguments"].(string)), &args)
	match := regexp.MustCompile(`PutAsync\('([^']+)'`).FindStringSubmatch(args.Cmd)
	if len(match) != 2 {
		t.Fatal(args.Cmd)
	}
	return call, match[1]
}

func attachmentPut(x *attachmentReadRig, destination string, data []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest("PUT", destination, bytes.NewReader(data))
	w := httptest.NewRecorder()
	x.s.AttachmentUploadHandler().ServeHTTP(w, r)
	return w
}

func attachReceipt(body map[string]any, call map[string]any, output string) {
	body["input"] = append(body["input"].([]any), call, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "Chunk ID: test\nProcess exited with code 0\nOutput:\n" + output})
}

func TestAttachmentReadBoundaries(t *testing.T) {
	for _, tc := range []string{"other-caller", "other-session", "changed-history", "changed-command", "fake-receipt", "denied", "expired", "duplicate-output"} {
		t.Run(tc, func(t *testing.T) {
			x := newAttachmentReadRig(t)
			body := attachmentReadBody("C:/selected/test.xls")
			call, destination := issuedAttachment(t, x, body)
			receipt := "denied by native client policy"
			if tc != "denied" {
				up := attachmentPut(x, destination, []byte("synthetic"))
				if up.Code != 200 {
					t.Fatal(up.Body)
				}
				receipt = up.Body.String()
			}
			attachReceipt(body, call, receipt)
			owner := "caller-A"
			switch tc {
			case "other-caller":
				owner = "caller-B"
			case "other-session":
				body["session_id"] = "session-B"
			case "changed-history":
				body["input"].([]any)[0].(map[string]any)["content"] = strings.ReplaceAll(body["input"].([]any)[0].(map[string]any)["content"].(string), "test.xls", "other.xls")
			case "changed-command":
				call["arguments"] = `{"cmd":"do something else"}`
			case "fake-receipt":
				body["input"].([]any)[2].(map[string]any)["output"] = `{"read_id":"forged","bytes":9,"sha256":"forged"}`
			case "expired":
				x.s.attachmentReads[call["call_id"].(string)].expires = time.Now().Add(-time.Second)
			case "duplicate-output":
				body["input"] = append(body["input"].([]any), body["input"].([]any)[2])
			}
			w := x.post(t, owner, body)
			if w.Code < 400 || x.calls != 0 || strings.Contains(w.Body.String(), "C:/selected") {
				t.Fatalf("unsafe continuation %d %s upstream=%d", w.Code, w.Body, x.calls)
			}
		})
	}
}

func TestAttachmentUploadLimits(t *testing.T) {
	for _, tc := range []string{"oversize", "unknown-size", "incomplete", "origin", "reused", "expired", "budget", "empty"} {
		t.Run(tc, func(t *testing.T) {
			x := newAttachmentReadRig(t)
			x.s.maxUpload = 16
			body := attachmentReadBody("C:/selected/test.xls")
			call, destination := issuedAttachment(t, x, body)
			r := httptest.NewRequest("PUT", destination, bytes.NewReader([]byte("synthetic")))
			want := 400
			switch tc {
			case "oversize":
				r.ContentLength = 17
				want = 413
			case "unknown-size":
				r.ContentLength = -1
				want = 413
			case "incomplete":
				r.ContentLength = 12
			case "origin":
				r.Header.Set("Origin", "https://elsewhere.example")
				want = 403
			case "reused":
				if attachmentPut(x, destination, []byte("synthetic")).Code != 200 {
					t.Fatal("initial upload")
				}
				want = 409
			case "expired":
				x.s.attachmentReads[call["call_id"].(string)].expires = time.Now().Add(-time.Second)
				want = 404
			case "budget":
				x.s.attachmentBytes = attachmentReadBudget - 8
				want = 429
			case "empty":
				r = httptest.NewRequest("PUT", destination, bytes.NewReader(nil))
				want = 200
			}
			w := httptest.NewRecorder()
			x.s.AttachmentUploadHandler().ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("HTTP %d want %d: %s", w.Code, want, w.Body)
			}
			if tc == "incomplete" && x.s.attachmentBytes != 0 {
				t.Fatal("incomplete bytes retained")
			}
		})
	}
}

func TestAttachmentIntakeTrustAndPolicies(t *testing.T) {
	for _, tc := range []string{"none", "required", "named", "unknown-tool", "relative", "unc", "directory", "duplicate", "untrusted-host", "missing-origin", "http-origin", "query-origin", "userinfo-origin"} {
		t.Run(tc, func(t *testing.T) {
			x := newAttachmentReadRig(t)
			body := attachmentReadBody("C:/selected/test.xls")
			switch tc {
			case "none", "required":
				body["tool_choice"] = tc
			case "named":
				body["tool_choice"] = map[string]string{"type": "function", "name": "exec_command"}
			case "unknown-tool":
				body["tools"].([]any)[0].(map[string]any)["name"] = "arbitrary_exec"
			case "relative", "unc", "directory", "duplicate":
				p := map[string]string{"relative": "relative.xls", "unc": "//server/share/file.xls", "directory": "C:/selected/", "duplicate": "C:/selected/test.xls\n## duplicate: C:/selected/test.xls"}[tc]
				body = attachmentReadBody(p)
			case "untrusted-host", "missing-origin":
				x.s.publicBaseURL = ""
			case "http-origin":
				x.s.publicBaseURL = "http://untrusted.example"
			case "query-origin":
				x.s.publicBaseURL = "https://trusted.example?next=evil"
			case "userinfo-origin":
				x.s.publicBaseURL = "https://user:password@trusted.example"
			}
			b, _ := json.Marshal(body)
			r := httptest.NewRequest("POST", "https://evil.example/v1/responses", bytes.NewReader(b))
			r.Header.Set("X-Forwarded-Host", "trusted.example")
			r = r.WithContext(WithEmbeddedAccount(r.Context(), SourceAccount{ID: "fixture-account", Cookie: "fixture-cookie"}))
			w := httptest.NewRecorder()
			x.s.CallerHandler("caller-A").ServeHTTP(w, r)
			if w.Code < 400 || x.calls != 0 || len(x.s.attachmentReads) != 0 {
				t.Fatalf("unexpected dispatch: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestAttachmentTransportHistoryAfterExpiryAndRestart(t *testing.T) {
	x := newAttachmentReadRig(t)
	body := attachmentReadBody("C:/selected/test.xls")
	call, destination := issuedAttachment(t, x, body)
	receipt := attachmentPut(x, destination, []byte("ATTACHMENT_HIDDEN_test"))
	attachReceipt(body, call, receipt.Body.String())
	if w := x.post(t, "caller-A", body); w.Code != 200 {
		t.Fatal(w.Body)
	}
	x.s.attachmentReads[call["call_id"].(string)].expires = time.Now().Add(-time.Second)
	x.s.expireAttachmentReads()
	if x.s.attachmentBytes != 0 || len(x.s.attachmentReads) != 0 {
		t.Fatal("expired bytes retained")
	}
	body["input"] = append(body["input"].([]any), map[string]any{"role": "assistant", "content": "seen"}, map[string]any{"role": "user", "content": "Continue discussing the file"})
	w := x.post(t, "caller-A", body)
	if w.Code != 200 || w.Header().Get("X-Hark-Project-Reused") != "true" {
		t.Fatalf("expired history reset: %d %s", w.Code, w.Body)
	}
	var sent string
	json.Unmarshal(x.sent["message"], &sent)
	if strings.Contains(sent, "attachment-reads") || strings.Contains(sent, call["call_id"].(string)) {
		t.Fatal("transport history leaked")
	}
	restarted, err := NewEmbedded(x.s.data, Options{Upstream: x.s.upstream})
	if err != nil {
		t.Fatal(err)
	}
	x.s = restarted
	w = x.post(t, "caller-A", body)
	if w.Code != 200 {
		t.Fatalf("restart did not recognize signed historical transport: %d %s", w.Code, w.Body)
	}
}

func TestAttachmentReadMultipleFiles(t *testing.T) {
	x := newAttachmentReadRig(t)
	body := attachmentReadBody("C:/selected/a.xls\n## second.pdf: C:/selected/b.pdf")
	for _, text := range []string{"first-file", "ATTACHMENT_HIDDEN_second"} {
		call, destination := issuedAttachment(t, x, body)
		up := attachmentPut(x, destination, []byte(text))
		if up.Code != 200 {
			t.Fatal(up.Body)
		}
		attachReceipt(body, call, up.Body.String())
	}
	w := x.post(t, "caller-A", body)
	if w.Code != 200 || w.Header().Get("X-Hark-Uploaded-Files") != "2" || !strings.Contains(w.Body.String(), "ATTACHMENT_HIDDEN_second") {
		t.Fatalf("multi-file continuation: HTTP %d headers=%v %s", w.Code, w.Header(), w.Body)
	}
}

func TestAttachmentPreflightPreservesInputAndExcludesTransportBilling(t *testing.T) {
	x := newAttachmentReadRig(t)
	body := attachmentReadBody("C:/selected/test.xls")
	body["model"] = "claude-opus-5-5"
	post := func(body map[string]any) (bool, []byte, *httptest.ResponseRecorder) {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(b))
		w := httptest.NewRecorder()
		handled, billing := x.s.PreflightAttachmentRead(w, r, "caller-A", "https://trusted.example")
		after, _ := io.ReadAll(r.Body)
		if !bytes.Equal(b, after) {
			t.Fatal("request body or model alias changed")
		}
		return handled, billing, w
	}
	handled, _, w := post(body)
	if !handled || w.Header().Get(AttachmentIntakeHeader) == "" {
		t.Fatal("intake entered model path")
	}
	call := attachmentReadCall(t, w)
	var args struct {
		Cmd string `json:"cmd"`
	}
	json.Unmarshal([]byte(call["arguments"].(string)), &args)
	destination := regexp.MustCompile(`PutAsync\('([^']+)'`).FindStringSubmatch(args.Cmd)[1]
	upload := attachmentPut(x, destination, []byte("synthetic"))
	attachReceipt(body, call, upload.Body.String())
	handled, billing, w := post(body)
	if handled || strings.Contains(string(billing), attachmentCallPrefix) || strings.Contains(string(billing), "read_id") || !strings.Contains(string(billing), "claude-opus-5-5") {
		t.Fatalf("billing projection: handled=%t HTTP %d %s", handled, w.Code, billing)
	}
	ordinary := map[string]any{"model": "hark", "input": "ordinary chat"}
	if handled, billing, _ := post(ordinary); handled || billing != nil {
		t.Fatal("ordinary chat intercepted")
	}
}

func TestAttachmentReadLeavesUploadedFilesAndNonUserPathsAlone(t *testing.T) {
	x := newAttachmentReadRig(t)
	for _, v := range []chatRequest{
		{Messages: []chatMessage{{Role: "user", Content: "C:/not-an-attachment.txt"}}},
		{Messages: []chatMessage{{Role: "user", Content: "hello"}, {Role: "tool", Content: "# Files mentioned by the user:\n## file: C:/private.txt\n## My request:\nread"}}},
		{Messages: []chatMessage{{Role: "user", Content: "# Files mentioned by the user:\n## file: C:/selected/test.xls\n## My request:\nread", Files: []inputFile{newInputFile("already.xls", "", []byte("already"), "file")}}}},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/responses", nil).WithContext(context.Background())
		handled, release := x.s.prepareAttachmentRead(w, r, &v)
		release()
		if handled || len(x.s.attachmentReads) != 0 {
			t.Fatal("unrelated message intercepted")
		}
	}
}

type gatedAttachmentReader struct {
	started, proceed chan struct{}
	reader           *bytes.Reader
	entered          bool
}

func (g *gatedAttachmentReader) Read(p []byte) (int, error) {
	if !g.entered {
		g.entered = true
		close(g.started)
		<-g.proceed
	}
	return g.reader.Read(p)
}

func TestAttachmentUploadReservesBeforeReading(t *testing.T) {
	x := newAttachmentReadRig(t)
	_, first := issuedAttachment(t, x, attachmentReadBody("C:/selected/a.bin"))
	_, second := issuedAttachment(t, x, attachmentReadBody("C:/selected/b.bin"))
	x.s.attachmentBytes = attachmentReadBudget - 8
	g := &gatedAttachmentReader{started: make(chan struct{}), proceed: make(chan struct{}), reader: bytes.NewReader([]byte("12345678"))}
	r := httptest.NewRequest("PUT", first, g)
	r.ContentLength = 8
	done := make(chan *httptest.ResponseRecorder)
	go func() { w := httptest.NewRecorder(); x.s.AttachmentUploadHandler().ServeHTTP(w, r); done <- w }()
	<-g.started
	if w := attachmentPut(x, second, []byte("x")); w.Code != 429 {
		t.Fatalf("in-flight reservation ignored: %d %s", w.Code, w.Body)
	}
	close(g.proceed)
	if w := <-done; w.Code != 200 {
		t.Fatalf("reserved upload failed: %d %s", w.Code, w.Body)
	}
	x.s.attachmentBytes = 8
}

func TestAttachmentBorrowPreventsEarlyExpiry(t *testing.T) {
	x := newAttachmentReadRig(t)
	body := attachmentReadBody("C:/selected/a.bin")
	call, destination := issuedAttachment(t, x, body)
	up := attachmentPut(x, destination, []byte("12345678"))
	attachReceipt(body, call, up.Body.String())
	b, _ := json.Marshal(body)
	v, err := x.s.normalizeResponses(b)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/responses", nil).WithContext(context.WithValue(context.Background(), recoveryCallerKey{}, "caller-A"))
	w := httptest.NewRecorder()
	handled, release := x.s.prepareAttachmentRead(w, r, &v)
	if handled {
		t.Fatal(w.Body)
	}
	a := x.s.attachmentReads[call["call_id"].(string)]
	a.expires = time.Now().Add(-time.Second)
	x.s.expireAttachmentReads()
	if x.s.attachmentBytes != 8 {
		t.Fatal("borrowed buffer uncharged early")
	}
	release()
	if x.s.attachmentBytes != 0 || len(x.s.attachmentReads) != 0 {
		t.Fatal("released expired bytes retained")
	}
}

func TestAttachmentUploadExpiringDuringReadDiscardsBytes(t *testing.T) {
	x := newAttachmentReadRig(t)
	body := attachmentReadBody("C:/selected/a.bin")
	call, destination := issuedAttachment(t, x, body)
	g := &gatedAttachmentReader{started: make(chan struct{}), proceed: make(chan struct{}), reader: bytes.NewReader([]byte("12345678"))}
	r := httptest.NewRequest("PUT", destination, g)
	r.ContentLength = 8
	done := make(chan *httptest.ResponseRecorder)
	go func() { w := httptest.NewRecorder(); x.s.AttachmentUploadHandler().ServeHTTP(w, r); done <- w }()
	<-g.started
	x.s.attachmentMu.Lock()
	x.s.attachmentReads[call["call_id"].(string)].expires = time.Now().Add(-time.Second)
	x.s.expireAttachmentReads()
	x.s.attachmentMu.Unlock()
	close(g.proceed)
	if w := <-done; w.Code != 410 {
		t.Fatalf("expired upload retained: %d %s", w.Code, w.Body)
	}
	if x.s.attachmentBytes != 0 || len(x.s.attachmentReads) != 0 {
		t.Fatal("expired upload retained bytes")
	}
}

func TestAttachmentReadPollsOnlyIssuedUpload(t *testing.T) {
	x := newAttachmentReadRig(t)
	body := attachmentReadBody("C:/selected/a.bin")
	body["tools"] = append(body["tools"].([]any), map[string]any{"type": "function", "name": "write_stdin", "parameters": json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"integer"},"chars":{"type":"string"},"yield_time_ms":{"type":"integer"}},"required":["session_id"]}`)})
	call, destination := issuedAttachment(t, x, body)
	attachReceipt(body, call, "Chunk ID: process\nWall time: 30 seconds\nProcess running with session ID 12345\nOutput:\n")
	// The native running status is wrapper metadata, not stdout or a fabricated receipt.
	body["input"].([]any)[2].(map[string]any)["output"] = "Chunk ID: process\nWall time: 30 seconds\nProcess running with session ID 12345\nOutput:\n"
	w := x.post(t, "caller-A", body)
	var env struct {
		Output []map[string]any `json:"output"`
	}
	json.Unmarshal(w.Body.Bytes(), &env)
	if w.Code != 200 || len(env.Output) != 1 || env.Output[0]["name"] != "write_stdin" {
		t.Fatalf("poll dispatch: HTTP %d %s", w.Code, w.Body)
	}
	poll := env.Output[0]
	if strings.Contains(poll["arguments"].(string), "require_escalated") || !strings.Contains(poll["arguments"].(string), `"chars":""`) {
		t.Fatal("poll can write or escalate")
	}
	up := attachmentPut(x, destination, []byte("ATTACHMENT_HIDDEN_poll"))
	attachReceipt(body, poll, up.Body.String())
	w = x.post(t, "caller-A", body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ATTACHMENT_HIDDEN_poll") {
		t.Fatalf("poll continuation: HTTP %d %s", w.Code, w.Body)
	}
}

func TestAttachmentCommandQuotesPowerShellDelimiters(t *testing.T) {
	file := "C:/selected/a'\u2018\u2019\u201a\u201b; Write-Output INJECTED; #.xls"
	command := attachmentCommand(file, "https://trusted.example/v1/attachment-reads/fixture", 20<<20)
	for _, literal := range []string{"''", "\u2018\u2018", "\u2019\u2019", "\u201a\u201a", "\u201b\u201b"} {
		if !strings.Contains(command, literal) {
			t.Fatalf("delimiter not escaped: %q", literal)
		}
	}
}

func TestAttachmentTransportStrippedForCompaction(t *testing.T) {
	x := newAttachmentReadRig(t)
	body := attachmentReadBody("C:/selected/a.bin")
	call, _ := issuedAttachment(t, x, body)
	attachReceipt(body, call, "historical receipt")
	body["input"] = append(body["input"].([]any), map[string]any{"role": "user", "content": "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.\n\nInclude:\n- Current progress and key decisions made\n- Important context, constraints, and user preferences\n- What remains to be done (clear next steps)\n- Any critical data, examples, or references needed to continue\n\nBe concise, structured, and focused on helping the next LLM seamlessly continue the work."})
	b, _ := json.Marshal(body)
	v, err := x.s.normalizeResponses(b)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/responses", nil).WithContext(context.WithValue(context.Background(), recoveryCallerKey{}, "caller-A"))
	w := httptest.NewRecorder()
	handled, release := x.s.prepareAttachmentRead(w, r, &v)
	defer release()
	if handled {
		t.Fatal(w.Body)
	}
	for _, m := range v.Messages {
		if m.ToolCalls != "" || m.Role == "tool" {
			t.Fatal("historical upload exposed to checkpoint")
		}
	}
}

func TestAttachmentCachedPollHonorsCurrentPolicy(t *testing.T) {
	for _, policy := range []string{"none", "required", "named", "removed-tool"} {
		t.Run(policy, func(t *testing.T) {
			x := newAttachmentReadRig(t)
			body := attachmentReadBody("C:/selected/a.bin")
			body["tools"] = append(body["tools"].([]any), map[string]any{"type": "function", "name": "write_stdin", "parameters": json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"integer"},"chars":{"type":"string"}},"required":["session_id"]}`)})
			call, _ := issuedAttachment(t, x, body)
			attachReceipt(body, call, "unused")
			body["input"].([]any)[2].(map[string]any)["output"] = "Chunk ID: process\nProcess running with session ID 12345\nOutput:\n"
			if w := x.post(t, "caller-A", body); w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"write_stdin"`) {
				t.Fatal("initial poll not issued")
			}
			switch policy {
			case "none", "required":
				body["tool_choice"] = policy
			case "named":
				body["tool_choice"] = map[string]string{"type": "function", "name": "exec_command"}
			case "removed-tool":
				body["tools"] = body["tools"].([]any)[:1]
			}
			w := x.post(t, "caller-A", body)
			if w.Code < 400 || strings.Contains(w.Body.String(), `"type":"function_call"`) || x.calls != 0 {
				t.Fatalf("cached poll ignored current policy: %d %s", w.Code, w.Body)
			}
		})
	}
}
