package appleid

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestWorkbenchResponseAndCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	calls := 0
	h := &workbenchHandler{bootstrap: func(context.Context) (*Challenge, error) {
		calls++
		return &Challenge{ImageBase64: "fixture", MIMEType: "image/jpeg", ImageBytes: 8}, nil
	}}
	router := gin.New()
	router.POST("/bootstrap", h.initialize)
	for _, want := range []int{200, 429} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/bootstrap", nil))
		if rec.Code != want || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unexpected response: %d %s", rec.Code, rec.Body.String())
		}
		if want == 429 && rec.Header().Get("Retry-After") != "5" {
			t.Fatal("retry interval missing")
		}
	}
	if calls != 1 {
		t.Fatal("duplicate upstream call")
	}
}

func TestWorkbenchErrorDoesNotExposeUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &workbenchHandler{bootstrap: func(context.Context) (*Challenge, error) { return nil, errors.New("private-upstream-token") }}
	router := gin.New()
	router.POST("/bootstrap", h.initialize)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/bootstrap", nil))
	if rec.Code != 502 || strings.Contains(rec.Body.String(), "private-upstream-token") {
		t.Fatal("unexpected error response")
	}
}

func TestWorkbenchInheritsAdminAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/api/v1/admin")
	group.Use(func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) })
	RegisterRoutes(group)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/appleid/bootstrap", nil))
	if rec.Code != 401 {
		t.Fatalf("unauthenticated access: %d", rec.Code)
	}
}
