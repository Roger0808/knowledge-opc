package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// VAL-216 / BND-010: every route of the removed skill-release chain is gone
// from the registration. Hitting an old path must return a plain 404 — no
// redirect, no alias, no silent mapping onto the catalog archive.
func TestSkillReleaseRoutesAreGone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g := &rbacGuards{}
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Next() })
	v1 := engine.Group("/api/v1")
	RegisterSkillRoutes(v1, &handler.SkillHandler{}, g)

	oldPaths := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/skills/catalog/cat-1/publish"},
		{http.MethodGet, "/api/v1/skills/releases"},
		{http.MethodGet, "/api/v1/skills/releases/rel-1"},
		{http.MethodGet, "/api/v1/skills/releases/rel-1/archive"},
		{http.MethodPost, "/api/v1/skills/releases/rel-1/delist"},
	}
	for _, tc := range oldPaths {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		require.Equalf(t, http.StatusNotFound, w.Code, "%s %s must 404", tc.method, tc.path)
	}

	// No API-key capability may resurrect them either.
	for _, tc := range oldPaths {
		_, ok := g.apiKeyAuthorizer.Lookup(tc.method, tc.path)
		require.Falsef(t, ok, "%s %s must not be declared for API keys", tc.method, tc.path)
	}
}
