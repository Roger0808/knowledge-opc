package router

import (
	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/handler"
)

// RegisterIntegrationRoutes wires the EXT-002 request-level MCP invocation
// surface. The group deliberately stays off the apiKey* helpers: decisions
// must come from interactive users (JWT sessions), so X-API-Key principals
// are default-denied by the API-key authorizer. All routes are Viewer+ and
// sit behind the global Auth middleware like every other /api/v1 group.
func RegisterIntegrationRoutes(r *gin.RouterGroup, h *handler.MCPInvocationHandler, g *rbacGuards) {
	if h == nil {
		return
	}
	integration := r.Group("/integration/mcp")
	{
		// Create one precise tool invocation — Viewer+
		integration.POST("/invocations", g.Viewer(), h.CreateInvocation)
		// Status/result query, caller-scoped — Viewer+
		integration.GET("/invocations/:id", g.Viewer(), h.GetInvocation)
		// Cancel: pre-send cancelled, executing cancel_requested — Viewer+
		integration.POST("/invocations/:id/cancel", g.Viewer(), h.CancelInvocation)
		// Single-effective request-level decision — Viewer+ interactive user
		integration.POST("/invocations/:id/decision", g.Viewer(), h.DecideInvocation)
		// Tenant service/tool catalog ∩ policy ∩ context capability — Viewer+
		integration.GET("/services", g.Viewer(), h.ListServices)
	}
}
