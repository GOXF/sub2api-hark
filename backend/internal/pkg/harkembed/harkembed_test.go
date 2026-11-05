package harkembed

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseCredential(t *testing.T) {
	if c := ParseCredential(" a=b; c=d "); c.Cookie != "a=b; c=d" || c.UserAgent != "" {
		t.Fatalf("raw: %+v", c)
	}
	if c := ParseCredential(`{"cookie":"x=1","user_agent":"UA"}`); c.Cookie != "x=1" || c.UserAgent != "UA" {
		t.Fatalf("json: %+v", c)
	}
}

func TestHandles(t *testing.T) {
	r := httptest.NewRequest("POST", "http://hark.internal/v1/chat/completions", nil)
	if !Handles(r) {
		t.Fatal("hark.internal not handled")
	}
	if Handles(httptest.NewRequest("POST", "https://api.openai.com/v1/chat/completions", nil)) {
		t.Fatal("other host handled")
	}
}

func TestServeStreams(t *testing.T) {
	step := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(201)
		_, _ = w.Write([]byte("a"))
		<-step
		_, _ = w.Write([]byte("b"))
	})
	resp := serve(h, httptest.NewRequest("GET", "http://hark.internal/x", nil))
	if resp.StatusCode != 201 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("head: %d %v", resp.StatusCode, resp.Header)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || buf[0] != 'a' {
		t.Fatalf("first chunk: %q %v", buf, err)
	}
	close(step)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "b" {
		t.Fatalf("rest: %q", rest)
	}
}

func TestDoRejectsWithoutCookie(t *testing.T) {
	t.Setenv("HARK_DATA_DIR", t.TempDir())
	r := httptest.NewRequest("POST", "http://hark.internal/v1/chat/completions", nil)
	r.Header.Set("Content-Type", "application/json")
	resp, err := Do(r, "", 7)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestAccountTimezone(t *testing.T) {
	t.Setenv("HARK_TIMEZONE", "")
	if got := accountTimezone(Credential{}); got != DefaultTimezone {
		t.Fatalf("default = %q", got)
	}
	if got := accountTimezone(ParseCredential(`{"cookie":"x=1","timezone":"America/New_York"}`)); got != "America/New_York" {
		t.Fatalf("credential = %q", got)
	}
	t.Setenv("HARK_TIMEZONE", "America/Chicago")
	if got := accountTimezone(Credential{}); got != "America/Chicago" {
		t.Fatalf("env = %q", got)
	}
	if got := accountTimezone(Credential{Timezone: "Not/AZone"}); got != "America/Chicago" {
		t.Fatalf("invalid credential = %q", got)
	}
}
