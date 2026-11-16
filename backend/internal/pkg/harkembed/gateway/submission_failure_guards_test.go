package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSubmissionFailureEvidenceGuards(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		rejected   bool
	}{
		{"run-override-denied", `{"error":"run override not enabled"}`, 403, true},
		{"structured-error", `{"error":{"code":"invalid_request","message":"fixture"}}`, 422, true},
		{"proxy-html", `<html>403 blocked</html>`, 403, false},
		{"contradictory-success", `{"success":true,"error":"fixture"}`, 400, false},
		{"contradictory-accepted", `{"accepted":true,"error":"fixture"}`, 429, false},
		{"empty-error", `{"error":null}`, 400, false},
		{"server-after-possible-acceptance", `{"success":false,"error":"fixture"}`, 500, false},
		{"oversized", `{"error":"` + strings.Repeat("x", 65536) + `"}`, 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := explicitSubmissionRejection(tc.status, []byte(tc.body)); got != tc.rejected {
				t.Fatalf("rejection proof mismatch: %t", got)
			}
		})
	}
	t.Run("redacted-bounded-original", func(t *testing.T) {
		body := []byte(`{"error":"fixture failure","cookie":"sid=private-fixture-token","authorization":"Bearer private-auth-token","detail":"` + strings.Repeat("x", 70000) + `"}`)
		err := nativeSubmissionFailure(&upstreamError{502, "upstream_http_error", "original error retained"}, "response", 503, body, "sid=private-fixture-token")
		var failure *nativeSubmissionError
		if !errors.As(err, &failure) {
			t.Fatal("missing typed original cause")
		}
		f := failure.failure
		if f.Code != "upstream_http_error" || f.Message != "original error retained" || f.Outcome != "uncertain" || !f.ResponseCut || f.ResponseBytes != 65536 || f.ResponseHash != previewSHA(body[:65536]) {
			t.Fatal("bounded original response provenance incorrect")
		}
		if len(f.ResponseDetail) > 8192 || strings.Contains(f.ResponseDetail, "private-fixture-token") || strings.Contains(f.ResponseDetail, "private-auth-token") {
			t.Fatal("credential leaked into saved diagnostic excerpt")
		}
		if !validSubmissionFailure(&f) {
			t.Fatal("valid saved evidence rejected")
		}
		f.Outcome = "invented-accepted"
		if validSubmissionFailure(&f) {
			t.Fatal("corrupt classification accepted")
		}
	})
	t.Run("prepare-not-sent", func(t *testing.T) {
		err := nativeSubmissionFailure(fmt.Errorf("synthetic local encoding failure"), "prepare", 0, nil, "")
		var failure *nativeSubmissionError
		if !errors.As(err, &failure) || failure.failure.Outcome != "not_sent" || failure.failure.NativeStatus != 0 {
			t.Fatal("local preparation failure called ambiguous native acceptance")
		}
	})
	t.Run("proxy-credentials-redacted", func(t *testing.T) {
		got := safeSubmissionText("proxy socks5://fixture-user:fixture-password@127.0.0.1:9 connection failed", "", 8192)
		if strings.Contains(got, "fixture-user") || strings.Contains(got, "fixture-password") || !strings.Contains(got, "127.0.0.1:9") {
			t.Fatal("transport proxy credentials not redacted")
		}
	})
	t.Run("partial-response-not-rejection-proof", func(t *testing.T) {
		err := nativeSubmissionFailure(&upstreamError{400, "upstream_request_rejected", "original400"}, "read_response", 400, []byte(`{"error":"invalid_request"}`), "")
		var failure *nativeSubmissionError
		if !errors.As(err, &failure) || failure.failure.Outcome != "uncertain" || failure.failure.NativeStatus != 400 {
			t.Fatal("an interrupted response was treated as a full rejection proof")
		}
	})
	t.Run("wrapped-auth-stays-compatible", func(t *testing.T) {
		err := nativeSubmissionFailure(&upstreamError{502, "upstream_access_blocked", "original blocked detail"}, "response", 403, []byte(`{"error":"blocked"}`), "")
		if !isAuthError(err) {
			t.Fatal("typed provenance broke existing auth classification")
		}
		s := &Server{accounts: []Account{{ID: "fixture"}}}
		s.markAccountLocked(0, err)
		if s.accounts[0].Status != "blocked" || s.accounts[0].Detail != "original blocked detail" {
			t.Fatal("wrapped 403 was changed to an expired 401")
		}
	})
	t.Run("canceled-send-keeps-real-transport-error", func(t *testing.T) {
		s, err := NewEmbedded(t.TempDir(), Options{Upstream: "http://127.0.0.1:1"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err = s.harkJSON(ctx, "POST", "/api/messages/send?cid=fixture", "fixture-cookie", map[string]string{"message": "fixture"}, nil)
		var failure *nativeSubmissionError
		if !errors.As(err, &failure) || failure.failure.Outcome != "uncertain" || failure.failure.Phase != "send" || !strings.Contains(failure.failure.TransportError, "context canceled") {
			t.Fatal("transport error was lost or guessed as rejection")
		}
	})
}
