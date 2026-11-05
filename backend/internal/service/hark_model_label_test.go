//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

type harkModelCaptureUpstream struct{ model any }

func (u *harkModelCaptureUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.model = req.Context().Value(ctxkey.HarkRequestedModel)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
}

func (u *harkModelCaptureUpstream) DoWithTLS(req *http.Request, p string, id int64, c int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, id, c)
}

func TestDoOpenAIUpstreamCarriesHarkRequestedModel(t *testing.T) {
	newReq := func(ctx context.Context) *http.Request {
		return httptest.NewRequest(http.MethodPost, "http://hark.internal/v1/chat/completions", nil).WithContext(ctx)
	}
	clientCtx := context.WithValue(context.Background(), ctxkey.Model, "claude-opus-5-5")

	cases := []struct {
		name    string
		account *Account
		ctx     context.Context
		want    any
	}{
		{"hark account gets client model", &Account{ID: 1, Platform: PlatformHark}, clientCtx, "claude-opus-5-5"},
		{"composite public model fallback", &Account{ID: 1, Platform: PlatformHark}, context.WithValue(context.Background(), ctxkey.RequestedPublicModel, "gpt-5"), "gpt-5"},
		{"no client model", &Account{ID: 1, Platform: PlatformHark}, context.Background(), nil},
		{"openai account untouched", &Account{ID: 2, Platform: PlatformOpenAI}, clientCtx, nil},
	}
	for _, tc := range cases {
		up := &harkModelCaptureUpstream{}
		s := &OpenAIGatewayService{httpUpstream: up}
		resp, err := s.doOpenAIUpstream(newReq(tc.ctx), "", tc.account)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_ = resp.Body.Close()
		if up.model != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, up.model, tc.want)
		}
	}
}

func TestWithHarkRequestedModelDefaultsWithoutSettings(t *testing.T) {
	// Without a settings service labelling defaults to on with the built-in prompt.
	req := httptest.NewRequest(http.MethodPost, "http://hark.internal/v1/responses", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Model, "gpt-5"))
	out := withHarkRequestedModel(req, nil)
	if out.Context().Value(ctxkey.HarkRequestedModel) != "gpt-5" || out.Context().Value(ctxkey.HarkModelLabelPrompt) != nil {
		t.Fatalf("defaults: %v %v", out.Context().Value(ctxkey.HarkRequestedModel), out.Context().Value(ctxkey.HarkModelLabelPrompt))
	}
}
