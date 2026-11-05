package appleid

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// RegisterRoutes must be called on the existing authenticated admin group.
// The module has no account repository, global settings or database migrations.
func RegisterRoutes(admin *gin.RouterGroup) {
	h := &workbenchHandler{bootstrap: Bootstrap}
	admin.POST("/appleid/bootstrap", h.initialize)
}

type workbenchHandler struct {
	bootstrap   func(context.Context) (*Challenge, error)
	mu          sync.Mutex
	busy        bool
	nextAttempt time.Time
}

func (h *workbenchHandler) initialize(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	h.mu.Lock()
	if h.busy || time.Now().Before(h.nextAttempt) {
		h.mu.Unlock()
		c.Header("Retry-After", "5")
		response.ErrorWithDetails(c, http.StatusTooManyRequests, "Apple ID workbench is busy. Try again shortly.", "APPLE_ID_BUSY", nil)
		return
	}
	h.busy = true
	h.nextAttempt = time.Now().Add(5 * time.Second)
	h.mu.Unlock()
	defer func() { h.mu.Lock(); h.busy = false; h.mu.Unlock() }()

	challenge, err := h.bootstrap(c.Request.Context())
	if err != nil {
		// Do not forward upstream errors, URLs, session headers or response bodies.
		response.ErrorWithDetails(c, http.StatusBadGateway, "Apple ID initialization failed. Try again later.", "APPLE_ID_BOOTSTRAP_FAILED", nil)
		return
	}
	response.Success(c, challenge)
}
