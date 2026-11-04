package harkembed

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func readForced(t *testing.T, r *http.Request) string {
	t.Helper()
	if err := forceHarkModel(r); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if r.ContentLength != int64(len(b)) || r.Header.Get("Content-Length") != strconv.Itoa(len(b)) {
		t.Fatalf("content length %d / %q, body %d", r.ContentLength, r.Header.Get("Content-Length"), len(b))
	}
	if r.GetBody != nil {
		again, _ := r.GetBody()
		ab, _ := io.ReadAll(again)
		if string(ab) != string(b) {
			t.Fatalf("GetBody differs: %q vs %q", ab, b)
		}
	}
	return string(b)
}

func TestForceHarkModelRewritesEveryEntry(t *testing.T) {
	cases := []struct {
		path, body string
	}{
		{"/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stream":true}`},
		{"/v1/responses", `{"model":"claude-opus-5-5","input":"hi","instructions":"be brief"}`},
		{"/v1/messages", `{"model":"claude-opus-5-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/chat/completions", `{"model":"hark-custom-mapped","messages":[]}`},
		{"/v1/responses", `{"input":"no model"}`},
		{"/v1/messages", `{"model":123,"messages":[]}`},
		{"/v1/chat/completions", `{"model":"hark","messages":[]}`},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodPost, "http://hark.internal"+tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		got := readForced(t, r)
		if m := gjson.Get(got, "model"); m.Str != Model {
			t.Fatalf("%s %s: model = %s", tc.path, tc.body, m.Raw)
		}
		// Every other field is kept as sent.
		want := gjson.Parse(tc.body).Map()
		have := gjson.Parse(got).Map()
		for k, v := range want {
			if k != "model" && have[k].Raw != v.Raw {
				t.Fatalf("%s: field %s = %s, want %s", tc.path, k, have[k].Raw, v.Raw)
			}
		}
		if len(have) != len(want) && !(len(have) == len(want)+1 && !gjson.Get(tc.body, "model").Exists()) {
			t.Fatalf("%s: fields changed: %s", tc.path, got)
		}
	}
}

func TestForceHarkModelLeavesOtherRequestsAlone(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://hark.internal/v1/chat/completions", strings.NewReader("--x\r\nmodel: gpt-5\r\n"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	if err := forceHarkModel(r); err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(r.Body); string(b) != "--x\r\nmodel: gpt-5\r\n" {
		t.Fatalf("multipart body changed: %q", b)
	}

	r = httptest.NewRequest(http.MethodPost, "http://hark.internal/v1/chat/completions", strings.NewReader(`not json "model":"gpt-5"`))
	r.Header.Set("Content-Type", "application/json")
	if got := readForced(t, r); got != `not json "model":"gpt-5"` {
		t.Fatalf("invalid JSON changed: %q", got)
	}

	r = httptest.NewRequest(http.MethodGet, "http://hark.internal/v1/models", nil)
	if err := forceHarkModel(r); err != nil {
		t.Fatal(err)
	}
}

// End to end through Do: a non-hark model must get past the gateway's model
// check (it now fails later, on the unreachable proxy, instead of answering
// "当前模型别名为 hark").
func TestDoAcceptsAnyInboundModel(t *testing.T) {
	t.Setenv("HARK_DATA_DIR", t.TempDir())
	cases := map[string]string{
		"/v1/chat/completions": `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`,
		"/v1/responses":        `{"model":"claude-opus-5-5","input":"hi"}`,
		"/v1/messages":         `{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
	}
	for path, body := range cases {
		r := httptest.NewRequest(http.MethodPost, "http://hark.internal"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer sid=test")
		resp, err := Do(r, "http://127.0.0.1:1", 4242)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(out), "当前模型别名为 hark") {
			t.Fatalf("%s: model rejected: %d %s", path, resp.StatusCode, out)
		}
		if resp.StatusCode < 400 {
			t.Fatalf("%s: expected an upstream failure through the dead proxy, got %d %s", path, resp.StatusCode, out)
		}
	}
}
