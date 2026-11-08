//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHarkAcceptedBudgetFailureNeverReplaysOnAnotherAccount(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformHark, Type: AccountTypeAPIKey}
	svc := &OpenAIGatewayService{}
	body := []byte(`{"error":{"code":"upstream_budget_exhausted","message":"Hark generation budget exhausted (cost_capped)"}}`)
	require.False(t, svc.shouldFailoverOpenAIUpstreamResponse(account, http.StatusTooManyRequests, "budget exhausted", body))
	require.Nil(t, svc.failoverOpenAIUpstreamHTTPError(context.Background(), nil, account,
		&http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}, body, "budget exhausted", "claude-opus-4-6"))
	for _, payload := range [][]byte{
		[]byte(`{"type":"error","error":{"code":"upstream_budget_exhausted","message":"budget exhausted"}}`),
		[]byte(`{"type":"response.failed","response":{"error":{"code":"upstream_budget_exhausted","message":"budget exhausted"}}}`),
	} {
		require.False(t, openAIStreamFailedEventShouldFailover(payload, "budget exhausted"))
		require.False(t, openAIStreamErrorEventShouldFailover(payload, "budget exhausted"))
	}
}

func TestHarkPreGenerationRateLimitKeepsExistingFailover(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformHark, Type: AccountTypeAPIKey}
	svc := &OpenAIGatewayService{}
	body := []byte(`{"error":{"code":"upstream_rate_limited","message":"upstream rate limited"}}`)
	require.True(t, svc.shouldFailoverOpenAIUpstreamResponse(account, http.StatusTooManyRequests, "rate limited", body))
}
