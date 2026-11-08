package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayModels_WildcardMappingUsesConcretePublicCatalog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	claudeModels := claude.DefaultModelIDs()
	sonnetModels := make([]string, 0)
	opusModels := make([]string, 0)
	latestModels := make([]string, 0)
	for _, model := range claudeModels {
		if strings.HasPrefix(model, "claude-sonnet-") {
			sonnetModels = append(sonnetModels, model)
		}
		if strings.HasPrefix(model, "claude-opus-") {
			opusModels = append(opusModels, model)
		}
		if strings.HasSuffix(model, "-5") {
			latestModels = append(latestModels, model)
		}
	}

	tests := []struct {
		name            string
		platform        string
		accountPlatform string
		mapping         map[string]any
		allowlist       service.GroupModelAllowlist
		want            []string
	}{
		{
			name:      "Hark platform catch-all uses Claude catalog despite disabled saved selections",
			platform:  service.PlatformHark,
			mapping:   map[string]any{"*": "hark"},
			allowlist: service.GroupModelAllowlist{Enabled: false, Models: []string{"saved-only-model"}},
			want:      claudeModels,
		},
		{
			name:     "Hark platform mixed exact aliases and wildcard keep both concrete sources",
			platform: service.PlatformHark,
			mapping:  map[string]any{"team-claude": "hark", "claude-sonnet-*": "hark"},
			want:     append([]string{"team-claude"}, sonnetModels...),
		},
		{
			name:     "Hark platform exact-only mapping does not broaden to defaults",
			platform: service.PlatformHark,
			mapping:  map[string]any{"team-claude": "hark"},
			want:     []string{"team-claude"},
		},
		{
			name:      "Hark platform enabled group selection restricts catch-all and keeps explicit alias",
			platform:  service.PlatformHark,
			mapping:   map[string]any{"*": "hark"},
			allowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"team-claude", "claude-opus-*"}},
			want:      append([]string{"team-claude"}, opusModels...),
		},
		{
			name:      "Hark platform interior group wildcard expands against covered catalog",
			platform:  service.PlatformHark,
			mapping:   map[string]any{"*": "hark"},
			allowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"claude-*-5"}},
			want:      latestModels,
		},
		{
			name:      "Hark platform group allowlist does not broaden explicit account aliases",
			platform:  service.PlatformHark,
			mapping:   map[string]any{"team-claude": "hark"},
			allowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"claude-opus-4-6", "team-claude"}},
			want:      []string{"team-claude"},
		},
		{
			name:     "Hark platform unmatched prefix remains empty",
			platform: service.PlatformHark,
			mapping:  map[string]any{"unlisted-*": "hark"},
			want:     []string{},
		},
		{
			name:            "Hark platform composite account expands its concrete catalog",
			platform:        service.PlatformComposite,
			accountPlatform: service.PlatformHark,
			mapping:         map[string]any{"*": "hark"},
			want:            claudeModels,
		},
		{
			name:      "Anthropic catch-all uses Claude catalog despite disabled saved selections",
			platform:  service.PlatformAnthropic,
			mapping:   map[string]any{"*": "hark"},
			allowlist: service.GroupModelAllowlist{Enabled: false, Models: []string{"saved-only-model"}},
			want:      claudeModels,
		},
		{
			name:     "mixed exact aliases and wildcard keep both concrete sources",
			platform: service.PlatformAnthropic,
			mapping:  map[string]any{"team-claude": "hark", "claude-sonnet-*": "hark"},
			want:     append([]string{"team-claude"}, sonnetModels...),
		},
		{
			name:      "enabled group selection restricts catch-all and keeps explicit alias",
			platform:  service.PlatformAnthropic,
			mapping:   map[string]any{"*": "hark"},
			allowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"team-claude", "claude-opus-*"}},
			want:      append([]string{"team-claude"}, opusModels...),
		},
		{
			name:      "interior group wildcard expands without becoming a model ID",
			platform:  service.PlatformAnthropic,
			mapping:   map[string]any{"*": "hark"},
			allowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"claude-*-5"}},
			want:      latestModels,
		},
		{
			name:     "prefix mapping does not advertise other Claude families",
			platform: service.PlatformAnthropic,
			mapping:  map[string]any{"claude-sonnet-*": "hark"},
			want:     sonnetModels,
		},
		{
			name:     "unmatched prefix stays empty rather than broadening to defaults",
			platform: service.PlatformAnthropic,
			mapping:  map[string]any{"unlisted-*": "hark"},
			want:     []string{},
		},
		{
			name:      "unmatched group wildcard is not a selectable model",
			platform:  service.PlatformAnthropic,
			mapping:   map[string]any{"*": "hark"},
			allowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"unlisted-*"}},
			want:      []string{},
		},
		{
			name:     "OpenAI wildcard keeps existing Codex policy without Claude fallback",
			platform: service.PlatformOpenAI,
			mapping:  map[string]any{"*": "upstream"},
			want:     []string{},
		},
		{
			name:     "OpenAI exact-only mapping is unchanged",
			platform: service.PlatformOpenAI,
			mapping:  map[string]any{"company-openai": "upstream"},
			want:     []string{"company-openai"},
		},
		{
			name:     "Gemini wildcard does not add Claude defaults and keeps exact alias",
			platform: service.PlatformGemini,
			mapping:  map[string]any{"*": "upstream", "company-gemini": "upstream"},
			want:     []string{"company-gemini"},
		},
		{
			name:            "composite wildcard expands only within linked platform",
			platform:        service.PlatformComposite,
			accountPlatform: service.PlatformAnthropic,
			mapping:         map[string]any{"*": "hark"},
			want:            claudeModels,
		},
		{
			name:            "composite allowlist retains concrete alias covered by catch-all",
			platform:        service.PlatformComposite,
			accountPlatform: service.PlatformAnthropic,
			mapping:         map[string]any{"*": "hark"},
			allowlist:       service.GroupModelAllowlist{Enabled: true, Models: []string{"team-claude", "claude-opus-*"}},
			want:            append([]string{"team-claude"}, opusModels...),
		},
		{
			name:            "composite unmatched mapping does not resurrect every platform",
			platform:        service.PlatformComposite,
			accountPlatform: service.PlatformAnthropic,
			mapping:         map[string]any{"unlisted-*": "hark"},
			want:            []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const groupID int64 = 401
			accountPlatform := tt.accountPlatform
			if accountPlatform == "" {
				accountPlatform = tt.platform
			}
			h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
				byGroup: map[int64][]service.Account{
					groupID: {{
						ID:          1,
						Platform:    accountPlatform,
						Type:        service.AccountTypeAPIKey,
						Credentials: map[string]any{"model_mapping": tt.mapping},
					}},
				},
			})
			group := &service.Group{ID: groupID, Platform: tt.platform, ModelAllowlist: tt.allowlist}
			for _, endpoint := range []string{"models", "codex"} {
				t.Run(endpoint, func(t *testing.T) {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
					c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: group})
					want := tt.want
					ids := make([]string, 0)
					if endpoint == "models" {
						h.Models(c)
						var got gatewayModelsResponseForTest
						require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
						ids = modelIDsForTest(got.Data)
					} else {
						h.CodexModels(c)
						var got codexModelsResponseForTest
						require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
						for _, model := range got.Models {
							ids = append(ids, model.Slug)
						}
						want = append([]string{}, service.FilterCodexModelIDsForGroup(want, group)...)
					}
					require.Equal(t, http.StatusOK, rec.Code)
					for _, id := range ids {
						require.NotContains(t, id, "*", "routing patterns are not selectable models")
					}
					if tt.allowlist.Enabled {
						require.Equal(t, want, ids, "enabled selections preserve group order")
					} else {
						require.ElementsMatch(t, want, ids)
					}
				})
			}
		})
	}
}
