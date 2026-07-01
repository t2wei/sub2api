package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayModels_EmbeddingsRemainVisibleOutsideCodexCatalog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{service.PlatformGemini, service.PlatformComposite, service.PlatformOpenAI} {
		t.Run(platform, func(t *testing.T) {
			const groupID int64 = 7352
			h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{})
			group := &service.Group{ID: groupID, Platform: platform}
			request := func(handle gin.HandlerFunc) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
				c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: group})
				handle(c)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				return rec
			}

			var ordinary gatewayModelsResponseForTest
			require.NoError(t, json.Unmarshal(request(h.Models).Body.Bytes(), &ordinary))
			ordinaryIDs := modelIDsForTest(ordinary.Data)
			if platform != service.PlatformOpenAI {
				require.Contains(t, ordinaryIDs, "gemini-embedding-001")
			}
			if platform != service.PlatformGemini {
				require.Contains(t, ordinaryIDs, "text-embedding-3-large")
			}

			var codex codexModelsResponseForTest
			require.NoError(t, json.Unmarshal(request(h.CodexModels).Body.Bytes(), &codex))
			require.NotEmpty(t, codex.Models)
			for _, model := range codex.Models {
				require.NotContains(t, []string{
					"text-embedding-3-small", "text-embedding-3-large", "text-embedding-ada-002",
					"gemini-embedding-001", "text-embedding-004", "embedding-001",
				}, model.Slug)
			}
		})
	}
}
