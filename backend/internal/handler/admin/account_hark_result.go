package admin

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// GetHarkSessionResult reads a saved native result through the original
// account, without scheduling, sending, executing tools or updating a session.
// GET /api/v1/admin/accounts/:id/hark/sessions/:session_id/result
func (h *AccountHandler) GetHarkSessionResult(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if account == nil || account.ID != id || !account.IsHark() || account.Type != service.AccountTypeAPIKey {
		response.BadRequest(c, "Read-only recovery requires the original Hark API key account")
		return
	}
	// Only the embedded gateway owns these mappings. Never route the saved
	// result through an arbitrary account base URL or another account.
	base := strings.TrimSpace(account.GetCredential("base_url"))
	if base != "" && base != harkembed.BaseURL && base != harkembed.BaseURL+"/v1" && base != harkembed.BaseURL+"/" {
		response.BadRequest(c, "Read-only recovery requires the embedded Hark gateway")
		return
	}
	key := account.GetCredential("api_key")
	if harkembed.ParseCredential(key).Cookie == "" {
		response.BadRequest(c, "Original Hark account has no login cookie")
		return
	}
	proxyURL := ""
	if account.ProxyID != nil {
		proxy, err := h.adminService.GetProxy(c.Request.Context(), *account.ProxyID)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		if proxy == nil || proxy.ID != *account.ProxyID {
			response.BadRequest(c, "Original account proxy not found; refusing direct fallback")
			return
		}
		proxyURL = proxy.URL()
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	endpoint := harkembed.BaseURL + "/v1/sessions/" + url.PathEscape(c.Param("session_id")) + "/result"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		response.BadRequest(c, "Invalid session ID")
		return
	}
	req.URL.RawQuery = c.Request.URL.RawQuery
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := harkembed.Do(req, proxyURL, id)
	if err != nil {
		response.InternalError(c, "Read-only result lookup failed")
		return
	}
	defer resp.Body.Close()
	const limit = 64 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit {
		response.InternalError(c, "Read-only result response unavailable")
		return
	}
	// Keep this diagnostic JSON outside Chat/Responses/Messages tool protocols.
	c.Data(resp.StatusCode, "application/json; charset=utf-8", body)
}
