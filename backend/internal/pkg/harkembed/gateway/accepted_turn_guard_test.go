package gateway

import (
	"context"
	"net/http"
	"testing"
)

func TestAcceptedTurnFailureSurvivesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, code := range []string{"upstream_turn_interrupted", "session_turn_pending", "session_account_conflict"} {
		status, got, _ := turnError(ctx, &upstreamError{http.StatusConflict, code, "preserved"})
		if status != http.StatusConflict || got != code {
			t.Fatalf("accepted/request-bound failure became retryable: %d %s", status, got)
		}
	}
	status, code, _ := turnError(ctx, &upstreamError{502, "upstream_error", "before acceptance"})
	if status != 504 || code != "upstream_timeout" {
		t.Fatal("ordinary pre-acceptance timeout classification changed")
	}
}
