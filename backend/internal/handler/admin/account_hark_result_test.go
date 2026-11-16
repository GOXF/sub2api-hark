package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountHarkSessionResultBoundary(t *testing.T) {
	const route = "/api/v1/admin/accounts/:id/hark/sessions/:session_id/result"
	for _, tc := range []struct {
		name, id string
		account  *service.Account
		auth     bool
		status   int
	}{
		{name: "unauthenticated", id: "50", auth: true, status: 401},
		{name: "invalid-id", id: "not-a-number", status: 400},
		{name: "negative-id", id: "-50", status: 400},
		{name: "wrong-account-type", id: "50", account: &service.Account{ID: 50, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}, status: 400},
		{name: "no-cookie", id: "50", account: &service.Account{ID: 50, Platform: service.PlatformHark, Type: service.AccountTypeAPIKey}, status: 400},
		{name: "external-host-denied", id: "50", account: &service.Account{ID: 50, Platform: service.PlatformHark, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://unrelated.invalid", "api_key": "fixture-cookie"}}, status: 400},
		{name: "wrong-account-returned", id: "39", account: &service.Account{ID: 50, Platform: service.PlatformHark, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-cookie"}}, status: 400},
		{name: "unknown-mapping-not-replayed", id: "50", account: &service.Account{ID: 50, Platform: service.PlatformHark, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-cookie"}}, status: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HARK_DATA_DIR", t.TempDir())
			gin.SetMode(gin.TestMode)
			r := gin.New()
			if tc.auth {
				r.Use(gin.HandlerFunc(middleware.NewAdminAuthMiddleware(nil, nil, nil, nil)))
			}
			svc := newStubAdminService()
			svc.getAccountResult = tc.account
			h := NewAccountHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			r.GET(route, h.GetHarkSessionResult)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/"+tc.id+"/hark/sessions/handler-fixture-unknown/result", nil))
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			if !tc.auth {
				require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			}
		})
	}
	// Ordinary account-read permissions must not expose conversation contents.
	policy := adminaccess.Policy{Rules: []adminaccess.Rule{{Permissions: []string{"accounts.read"}, AllGroups: true, IncludeUngrouped: true, AccountTypes: []string{service.AccountTypeAPIKey}}}}
	require.False(t, adminaccess.RouteAllowed(policy, http.MethodGet, route))
}

func TestAccountHarkRecoveryAdminBoundary(t *testing.T) {
	for _, action := range []string{"prepare", "ack"} {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.Use(gin.HandlerFunc(middleware.NewAdminAuthMiddleware(nil, nil, nil, nil)))
		h := NewAccountHandler(newStubAdminService(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		pattern := "/api/v1/admin/accounts/:id/hark/sessions/:session_id/recovery/" + action
		if action == "prepare" {
			r.POST(pattern, h.PrepareHarkRecovery)
		} else {
			r.POST(pattern, h.AcknowledgeHarkRecovery)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/50/hark/sessions/fixture/recovery/"+action, strings.NewReader(`{}`)))
		require.Equal(t, 401, w.Code)
		policy := adminaccess.Policy{Rules: []adminaccess.Rule{{Permissions: []string{"accounts.read", "accounts.recover"}, AllGroups: true, IncludeUngrouped: true, AccountTypes: []string{service.AccountTypeAPIKey}}}}
		require.False(t, adminaccess.RouteAllowed(policy, http.MethodPost, pattern))
	}
}
