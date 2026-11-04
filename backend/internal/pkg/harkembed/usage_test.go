package harkembed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed/gateway"
)

func withUsageUpstream(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	old := UsageUpstream
	UsageUpstream = srv.URL
	t.Cleanup(func() { UsageUpstream = old; srv.Close() })
}

func TestQueryUsageParsesBillingSummary(t *testing.T) {
	withUsageUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/billing/summary" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Cookie"); got != "a=1; b=2" {
			t.Errorf("cookie = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "UA/1.0" {
			t.Errorf("user agent = %q", got)
		}
		_, _ = w.Write([]byte(`{"plan":{"name":"Pro"},"meters":{"harkTokens":{"dailyLimit":1000,"dailyUsed":250,"dailyResetsAt":"2026-11-05T00:00:00Z","poolLimit":50000,"poolUsed":12345.6,"poolResetsAt":"2026-12-01T08:00:00.123+08:00"}}}`))
	})
	u, err := QueryUsage(context.Background(), `{"cookie":"a=1; b=2","user_agent":"UA/1.0"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if u.Plan != "Pro" || u.DailyUsed != 250 || u.DailyLimit != 1000 || u.PoolUsed != 12345 || u.PoolLimit != 50000 {
		t.Fatalf("usage = %+v", u)
	}
	if u.DailyResetsAt == nil || !u.DailyResetsAt.Equal(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily reset = %v", u.DailyResetsAt)
	}
	if u.PoolResetsAt == nil || u.PoolResetsAt.Location() != time.UTC || u.PoolResetsAt.Hour() != 0 {
		t.Fatalf("pool reset = %v", u.PoolResetsAt)
	}
}

func TestQueryUsageDefaultUserAgentAndBareCookie(t *testing.T) {
	withUsageUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "sid=x" || r.Header.Get("User-Agent") != gateway.DefaultUserAgent {
			t.Errorf("headers = %v", r.Header)
		}
		_, _ = w.Write([]byte(`{"meters":{"harkTokens":{"dailyLimit":1,"dailyUsed":0,"poolLimit":2,"poolUsed":1}}}`))
	})
	u, err := QueryUsage(context.Background(), "sid=x", "")
	if err != nil || u.DailyResetsAt != nil || u.PoolUsed != 1 {
		t.Fatalf("usage = %+v, err = %v", u, err)
	}
}

func TestQueryUsageErrors(t *testing.T) {
	status := http.StatusUnauthorized
	body := ""
	withUsageUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	_, err := QueryUsage(context.Background(), "sid=x", "")
	var ue *UsageError
	if !errors.As(err, &ue) || ue.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v", err)
	}
	status, body = http.StatusOK, `{"meters":{}}`
	if _, err = QueryUsage(context.Background(), "sid=x", ""); !errors.As(err, &ue) || ue.StatusCode != http.StatusOK {
		t.Fatalf("missing meters err = %v", err)
	}
	if _, err = QueryUsage(context.Background(), "", ""); err == nil {
		t.Fatal("expected error for empty cookie")
	}
}
