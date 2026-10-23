package router

import (
	"net/http"
	"testing"

	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// WORK-RESOURCES-001 TASK-004/VAL-209 boundary: the catalog archive download
// is a real Viewer+ route, and it is not declared for API keys at all — the
// platform bridge reaches it with a user token, so a scoped or full-access
// key must stay default-deny rather than silently inheriting the Viewer
// ladder.
func TestSkillCatalogArchiveRouteIsViewerRoutedAndDefaultDenyForAPIKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g := &rbacGuards{}
	engine := gin.New()
	v1 := engine.Group("/api/v1")
	RegisterSkillRoutes(v1, &handler.SkillHandler{}, g)

	found := false
	for _, ri := range engine.Routes() {
		if ri.Method == http.MethodGet && ri.Path == "/api/v1/skills/catalog/:id/archive" {
			found = true
		}
	}
	require.True(t, found, "catalog archive route must be registered")

	_, ok := g.apiKeyAuthorizer.Lookup(http.MethodGet, "/api/v1/skills/catalog/:id/archive")
	require.False(t, ok, "archive download must stay default-deny for API keys")
}
