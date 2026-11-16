package service

import (
	"fmt"
	"net/http"
	"strings"

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
		case "upstream_turn_interrupted":
			status = http.StatusConflict
		case "upstream_generation_failed":
			status = http.StatusBadGateway
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

// These request-scoped failures happen before submission. Another credential
// cannot repair retained history or contradictory summary/tool requirements.
func isHarkRequestContextFailure(body []byte) bool {
	for _, path := range []string{"error.code", "response.error.code"} {
		if strings.HasPrefix(gjson.GetBytes(body, path).String(), "recovery_") {
			return true
		}
		switch gjson.GetBytes(body, path).String() {
		case "session_label_history_mismatch", "compaction_tool_choice_conflict", "session_turn_pending", "session_account_conflict", "session_owner_mismatch":
			return true
		}
	}
	return false
}

// Stream transport may already be HTTP 200. A request-scoped recovery failure
// must still end as a failure, without treating it as a new account failure.
func harkStreamFailure(body []byte) (int, error) {
	if !isHarkRequestContextFailure(body) {
		return harkAcceptedTurnFailure(body)
	}
	code := gjson.GetBytes(body, "error.code").String()
	if code == "" {
		code = gjson.GetBytes(body, "response.error.code").String()
	}
	status := http.StatusConflict
	switch code {
	case "session_owner_mismatch":
		status = http.StatusForbidden
	case "recovery_store_failed":
		status = http.StatusServiceUnavailable
	}
	return status, fmt.Errorf("%s: %s", code, extractUpstreamErrorMessage(body))
}

func writeHarkRequestContextError(c *gin.Context, status int, body []byte) error {
	code := gjson.GetBytes(body, "error.code").String()
	if code == "" {
		code = gjson.GetBytes(body, "response.error.code").String()
	}
	message := extractUpstreamErrorMessage(body)
	setOpsUpstreamError(c, status, message, "")
	MarkResponseCommitted(c)
	c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "code": code, "message": message}})
	return fmt.Errorf("hark request context %s: %s", code, message)
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
