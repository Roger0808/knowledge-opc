package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/application/service"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
)

// MCPInvocationHandler serves the EXT-002 request-level MCP invocation
// surface: precise per-tool calls with policy checks, request-level approval,
// cancellation and honest outcome receipts.
type MCPInvocationHandler struct {
	invocations *service.MCPInvocationService
}

// NewMCPInvocationHandler wires the handler; svc may be nil in tests that
// only exercise routing.
func NewMCPInvocationHandler(svc *service.MCPInvocationService) *MCPInvocationHandler {
	return &MCPInvocationHandler{invocations: svc}
}

type createInvocationRequest struct {
	ServiceID         string         `json:"service_id"         binding:"required"`
	ToolName          string         `json:"tool_name"          binding:"required"`
	Arguments         map[string]any `json:"arguments"`
	IdempotencyKey    string         `json:"idempotency_key"`
	InvocationContext string         `json:"invocation_context"`
}

// mcpInvocationErrorBody is the enumerated-code error envelope of this
// surface. It carries request_id so a client report can be traced back to
// the server log line.
func mcpInvocationErrorBody(c *gin.Context, code, message string) gin.H {
	body := gin.H{
		"success": false,
		"error":   gin.H{"code": code, "message": message},
	}
	if id, ok := c.Get(types.RequestIDContextKey.String()); ok {
		if s, ok := id.(string); ok && s != "" {
			body["request_id"] = s
		}
	}
	return body
}

func writeMCPInvocationError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, service.ErrMCPInvocationPolicyDenied):
		c.JSON(http.StatusForbidden,
			mcpInvocationErrorBody(c, service.MCPInvocationCodePolicyDenied, err.Error()))
	case errors.Is(err, service.ErrMCPInvocationContextInvalid):
		c.JSON(http.StatusForbidden,
			mcpInvocationErrorBody(c, service.MCPInvocationCodeContextInvalid, err.Error()))
	case errors.Is(err, service.ErrMCPInvocationDecisionConflict):
		c.JSON(http.StatusConflict,
			mcpInvocationErrorBody(c, service.MCPInvocationCodeDecisionConflict, err.Error()))
	case errors.Is(err, service.ErrMCPInvocationForbidden):
		c.JSON(http.StatusForbidden,
			mcpInvocationErrorBody(c, service.MCPInvocationCodeDecisionForbidden, err.Error()))
	case errors.Is(err, service.ErrMCPInvocationNotFound):
		_ = c.Error(apperrors.NewNotFoundError("mcp invocation not found"))
	case errors.Is(err, service.ErrMCPInvocationInvalid):
		_ = c.Error(apperrors.NewBadRequestError(err.Error()))
	case errors.Is(err, service.ErrMCPInvocationArgsKeyRequired):
		c.JSON(http.StatusServiceUnavailable,
			mcpInvocationErrorBody(c, service.MCPInvocationCodeArgsKeyRequired, err.Error()))
	default:
		return false
	}
	return true
}

// CreateInvocation godoc
// @Summary      Create a request-level MCP tool invocation
// @Description  Creates one precise MCP tool call: policy check, approval or
// @Description  OAuth wait, then execution with an honest outcome receipt.
// @Tags         Integration
// @Accept       json
// @Produce      json
// @Param        body  body  handler.createInvocationRequest  true  "Invocation request"
// @Success      200   {object}  handler.DataResponse  "Terminal or executing state"
// @Success      202   {object}  handler.DataResponse  "Waiting on approval or OAuth"
// @Failure      403   {object}  gin.H  "MCP_POLICY_DENIED or CONTEXT_INVALID"
// @Security     Bearer
// @Router       /integration/mcp/invocations [post]
func (h *MCPInvocationHandler) CreateInvocation(c *gin.Context) {
	if h.invocations == nil {
		_ = c.Error(apperrors.NewNotFoundError("mcp invocation service unavailable"))
		return
	}
	var req createInvocationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		_ = c.Error(apperrors.NewBadRequestError("service_id and tool_name are required"))
		return
	}
	caller := service.MCPInvocationCaller{
		TenantID:  c.GetUint64(types.TenantIDContextKey.String()),
		UserID:    invocationUserID(c),
		ActorKind: types.MCPInvocationActorUser,
	}
	rec, err := h.invocations.Invoke(c.Request.Context(), caller, service.MCPInvocationInput{
		ServiceID:      req.ServiceID,
		ToolName:       req.ToolName,
		Arguments:      req.Arguments,
		IdempotencyKey: req.IdempotencyKey,
		ContextJWT:     req.InvocationContext,
	})
	if err != nil {
		if rec != nil && errors.Is(err, service.ErrMCPInvocationPolicyDenied) {
			body := mcpInvocationErrorBody(c, service.MCPInvocationCodePolicyDenied, err.Error())
			body["data"] = rec
			c.JSON(http.StatusForbidden, body)
			return
		}
		if writeMCPInvocationError(c, err) {
			return
		}
		_ = c.Error(err)
		return
	}
	status := http.StatusOK
	body := gin.H{"success": true, "data": rec}
	switch rec.Status {
	case types.MCPInvocationStatusWaitingApproval:
		status = http.StatusAccepted
		body["code"] = service.MCPInvocationCodeApprovalRequired
	case types.MCPInvocationStatusWaitingOAuth:
		status = http.StatusAccepted
	}
	c.JSON(status, body)
}

// GetInvocation godoc
// @Summary      Read one MCP invocation
// @Description  Status and (before expires_at) result of one of the caller's
// @Description  invocations. After expires_at only the status remains and
// @Description  the call answers 410 INVOCATION_EXPIRED.
// @Tags         Integration
// @Produce      json
// @Param        id   path  string  true  "Invocation ID"
// @Success      200  {object}  handler.DataResponse
// @Failure      410  {object}  gin.H  "INVOCATION_EXPIRED"
// @Security     Bearer
// @Router       /integration/mcp/invocations/{id} [get]
func (h *MCPInvocationHandler) GetInvocation(c *gin.Context) {
	if h.invocations == nil {
		_ = c.Error(apperrors.NewNotFoundError("mcp invocation service unavailable"))
		return
	}
	rec, err := h.invocations.Get(
		c.Request.Context(),
		c.GetUint64(types.TenantIDContextKey.String()),
		invocationUserID(c),
		c.Param("id"),
	)
	if err != nil {
		if errors.Is(err, service.ErrMCPInvocationExpired) {
			body := mcpInvocationErrorBody(c, service.MCPInvocationCodeExpired, err.Error())
			body["error"].(gin.H)["details"] = gin.H{"status": rec.Status}
			c.JSON(http.StatusGone, body)
			return
		}
		if errors.Is(err, service.ErrMCPInvocationForbidden) {
			// A foreign row is indistinguishable from a missing one.
			_ = c.Error(apperrors.NewNotFoundError("mcp invocation not found"))
			return
		}
		if writeMCPInvocationError(c, err) {
			return
		}
		_ = c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": rec})
}

// CancelInvocation godoc
// @Summary      Cancel an MCP invocation
// @Description  Pre-send and parked states cancel deterministically; an
// @Description  executing call becomes cancel_requested and settles from
// @Description  the actual upstream receipt.
// @Tags         Integration
// @Produce      json
// @Param        id   path  string  true  "Invocation ID"
// @Success      200  {object}  handler.DataResponse
// @Failure      409  {object}  gin.H  "DECISION_CONFLICT (already terminal)"
// @Security     Bearer
// @Router       /integration/mcp/invocations/{id}/cancel [post]
func (h *MCPInvocationHandler) CancelInvocation(c *gin.Context) {
	if h.invocations == nil {
		_ = c.Error(apperrors.NewNotFoundError("mcp invocation service unavailable"))
		return
	}
	rec, err := h.invocations.Cancel(
		c.Request.Context(),
		c.GetUint64(types.TenantIDContextKey.String()),
		invocationUserID(c),
		c.Param("id"),
	)
	if err != nil {
		if errors.Is(err, service.ErrMCPInvocationForbidden) {
			_ = c.Error(apperrors.NewNotFoundError("mcp invocation not found"))
			return
		}
		if writeMCPInvocationError(c, err) {
			return
		}
		_ = c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": rec})
}

type decideInvocationRequest struct {
	Decision      string `json:"decision"       binding:"required"`
	ArgsDigest    string `json:"args_digest"    binding:"required"`
	PolicyVersion string `json:"policy_version" binding:"required"`
}

// DecideInvocation godoc
// @Summary      Approve or deny a waiting MCP invocation
// @Description  Records the single effective decision of a waiting_approval
// @Description  invocation; approval executes the bound call. Task contexts
// @Description  hold no decision rights. Replays return the stored decision
// @Description  without re-executing.
// @Tags         Integration
// @Accept       json
// @Produce      json
// @Param        id    path  string  true  "Invocation ID"
// @Param        body  body  handler.decideInvocationRequest  true  "Decision"
// @Success      200   {object}  handler.DataResponse
// @Failure      403   {object}  gin.H  "MCP_DECISION_FORBIDDEN (task actor)"
// @Failure      409   {object}  gin.H  "DECISION_CONFLICT"
// @Security     Bearer
// @Router       /integration/mcp/invocations/{id}/decision [post]
func (h *MCPInvocationHandler) DecideInvocation(c *gin.Context) {
	if h.invocations == nil {
		_ = c.Error(apperrors.NewNotFoundError("mcp invocation service unavailable"))
		return
	}
	var req decideInvocationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		_ = c.Error(apperrors.NewBadRequestError("decision, args_digest and policy_version are required"))
		return
	}
	rec, decision, err := h.invocations.Decide(
		c.Request.Context(),
		c.GetUint64(types.TenantIDContextKey.String()),
		invocationUserID(c),
		c.Param("id"),
		req.Decision,
		req.ArgsDigest,
		req.PolicyVersion,
	)
	if err != nil {
		if writeMCPInvocationError(c, err) {
			return
		}
		_ = c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"invocation": rec,
		"decision":   decision,
	}})
}

// ListServices godoc
// @Summary      List tenant MCP services callable via invocations
// @Description  The enabled services and tool directories the caller's
// @Description  context may invoke: directory ∩ standing policy ∩ actor
// @Description  capability.
// @Tags         Integration
// @Produce      json
// @Param        actor_kind  query  string  false  "user (default) or task"
// @Success      200  {object}  handler.DataResponse
// @Security     Bearer
// @Router       /integration/mcp/services [get]
func (h *MCPInvocationHandler) ListServices(c *gin.Context) {
	if h.invocations == nil {
		_ = c.Error(apperrors.NewNotFoundError("mcp invocation service unavailable"))
		return
	}
	actorKind := strings.TrimSpace(c.Query("actor_kind"))
	if actorKind != types.MCPInvocationActorTask {
		actorKind = types.MCPInvocationActorUser
	}
	services, err := h.invocations.Catalog(c.Request.Context(),
		c.GetUint64(types.TenantIDContextKey.String()), actorKind)
	if err != nil {
		_ = c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"services": services}})
}

// invocationUserID is the interactive user behind the request. API-key
// callers carry no user id; the empty string then fails ownership checks
// without widening anyone's visibility.
func invocationUserID(c *gin.Context) string {
	if id, ok := types.UserIDFromContext(c.Request.Context()); ok {
		return id
	}
	return ""
}
