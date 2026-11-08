package service

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// The remote turn was accepted before this budget limit was observed. Retrying
// its request on another account can duplicate work even without visible text.
func isHarkBudgetExhausted(body []byte) bool {
	for _, path := range []string{"error.code", "response.error.code"} {
		if gjson.GetBytes(body, path).String() == "upstream_budget_exhausted" {
			return true
		}
	}
	return false
}

func writeHarkBudgetError(c *gin.Context, body []byte) error {
	message := strings.TrimSpace(extractUpstreamErrorMessage(body))
	if message == "" {
		message = "Hark generation budget exhausted (cost_capped); continue explicitly with the saved conversation history"
	}
	setOpsUpstreamError(c, http.StatusTooManyRequests, message, "")
	MarkResponseCommitted(c)
	c.JSON(http.StatusTooManyRequests, gin.H{
		"type":  "error",
		"error": gin.H{"type": "rate_limit_error", "code": "upstream_budget_exhausted", "message": message},
	})
	return fmt.Errorf("hark accepted turn budget exhausted: %s", message)
}
