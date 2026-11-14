package service

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// These structured codes are emitted after Hark accepted the turn. Keep the
// failure and observed usage, but never ask the scheduler to replay that turn.
func harkAcceptedTurnFailure(body []byte) (int, error) {
	for _, path := range []string{"error.code", "response.error.code"} {
		code := gjson.GetBytes(body, path).String()
		status := http.StatusUnprocessableEntity
		switch code {
		case "upstream_budget_exhausted":
			status = http.StatusTooManyRequests
		case "tool_call_missing", "tool_call_mismatch", "upstream_compaction_declined", "upstream_declined":
		default:
			continue
		}
		return status, fmt.Errorf("%s: %s", code, extractUpstreamErrorMessage(body))
	}
	return 0, nil
}

func isHarkAcceptedTurnFailure(body []byte) bool {
	_, failure := harkAcceptedTurnFailure(body)
	return failure != nil
}

func writeHarkAcceptedTurnError(c *gin.Context, body []byte) error {
	if isHarkBudgetExhausted(body) {
		return writeHarkBudgetError(c, body)
	}
	status, failure := harkAcceptedTurnFailure(body)
	if failure == nil {
		return fmt.Errorf("expected a structured Hark accepted-turn failure")
	}
	code := gjson.GetBytes(body, "error.code").String()
	if code == "" {
		code = gjson.GetBytes(body, "response.error.code").String()
	}
	message := extractUpstreamErrorMessage(body)
	setOpsUpstreamError(c, status, message, "")
	MarkResponseCommitted(c)
	c.JSON(status, gin.H{
		"type":  "error",
		"error": gin.H{"type": "api_error", "code": code, "message": message},
	})
	return failure
}
