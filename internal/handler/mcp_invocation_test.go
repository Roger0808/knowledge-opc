package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/mcp"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// --- fakes (handler-level mirror of the service fixture) -------------------

type fakeHandlerMCPRepo struct {
	interfaces.MCPServiceRepository
	services map[string]*types.MCPService
	metadata map[string]*types.MCPMetadata
}

func hKey(parts ...string) string { return joinParts(parts) }

func joinParts(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\x00"
		}
		out += p
	}
	return out
}

func (f *fakeHandlerMCPRepo) GetByID(_ context.Context, tenantID uint64, id string) (*types.MCPService, error) {
	return f.services[hKey(strconv.FormatUint(tenantID, 10), id)], nil
}

func (f *fakeHandlerMCPRepo) ListEnabled(_ context.Context, tenantID uint64) ([]*types.MCPService, error) {
	out := []*types.MCPService{}
	for _, svc := range f.services {
		if svc.TenantID == tenantID && svc.Enabled {
			out = append(out, svc)
		}
	}
	return out, nil
}

func (f *fakeHandlerMCPRepo) GetMetadata(
	_ context.Context, tenant uint64, service, principal string,
) (*types.MCPMetadata, error) {
	return f.metadata[hKey(strconv.FormatUint(tenant, 10), service, principal)], nil
}

func (f *fakeHandlerMCPRepo) ListMetadataSummaries(context.Context, uint64, []string) ([]*types.MCPMetadataSummary, error) {
	return nil, nil
}

func (f *fakeHandlerMCPRepo) SaveMetadata(context.Context, *types.MCPMetadata) error { return nil }

type fakeHandlerPolicyRepo struct {
	mu   sync.Mutex
	rows map[string]*types.MCPToolApproval
}

func (f *fakeHandlerPolicyRepo) Policy(_ context.Context, tenantID uint64, serviceID, toolName string) (*types.MCPToolApproval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[hKey(strconv.FormatUint(tenantID, 10), serviceID, toolName)], nil
}

func (f *fakeHandlerPolicyRepo) SetPolicy(_ context.Context, tenantID uint64, serviceID, toolName string, patch types.MCPToolPolicyPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := hKey(strconv.FormatUint(tenantID, 10), serviceID, toolName)
	row := f.rows[key]
	if row == nil {
		row = &types.MCPToolApproval{
			ID: "pol-" + toolName, TenantID: tenantID, ServiceID: serviceID, ToolName: toolName, Enabled: true,
		}
		f.rows[key] = row
	}
	if patch.RequireApproval != nil {
		row.RequireApproval = *patch.RequireApproval
	}
	if patch.Enabled != nil {
		row.Enabled = *patch.Enabled
	}
	row.UpdatedAt = time.Now()
	return nil
}

func (f *fakeHandlerPolicyRepo) ListByService(context.Context, uint64, string) ([]*types.MCPToolApproval, error) {
	return nil, nil
}
func (f *fakeHandlerPolicyRepo) SetRequireApproval(ctx context.Context, t uint64, s, tool string, v bool) error {
	return f.SetPolicy(ctx, t, s, tool, types.MCPToolPolicyPatch{RequireApproval: &v})
}
func (f *fakeHandlerPolicyRepo) SetEnabled(ctx context.Context, t uint64, s, tool string, v bool) error {
	return f.SetPolicy(ctx, t, s, tool, types.MCPToolPolicyPatch{Enabled: &v})
}
func (f *fakeHandlerPolicyRepo) IsRequired(context.Context, uint64, string, string) (bool, error) {
	return false, nil
}
func (f *fakeHandlerPolicyRepo) IsEnabled(context.Context, uint64, string, string) (bool, error) {
	return true, nil
}
func (f *fakeHandlerPolicyRepo) ObserveRisk(context.Context, uint64, string, string, string) error {
	return nil
}

type fakeHandlerOAuthRepo struct {
	interfaces.MCPOAuthRepository
	tokens map[string]*types.MCPOAuthToken
}

func (f *fakeHandlerOAuthRepo) GetTokenForPrincipal(
	_ context.Context, tenantID uint64, principal types.Principal, serviceID string,
) (*types.MCPOAuthToken, error) {
	return f.tokens[hKey(strconv.FormatUint(tenantID, 10), principal.StorageID(), serviceID)], nil
}

type fakeHandlerExecutor struct {
	mu     sync.Mutex
	calls  int
	result *mcp.CallToolResult
	err    error
}

func (f *fakeHandlerExecutor) CallTool(
	context.Context, *types.MCPService, string, map[string]any,
) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.result, f.err
}

func (f *fakeHandlerExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// --- fixture ---------------------------------------------------------------

const (
	hTenant = uint64(7)
	hUser   = "user-1"
	hKeySig = "handler-context-test-key"
	hAud    = "opc-invocations"
)

type invocationHandlerFixture struct {
	db     *gorm.DB
	mcp    *fakeHandlerMCPRepo
	pol    *fakeHandlerPolicyRepo
	oauth  *fakeHandlerOAuthRepo
	exec   *fakeHandlerExecutor
	router *gin.Engine
	jti    int
}

func newInvocationHandlerFixture(t *testing.T) *invocationHandlerFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:mcp-invocation-handler-"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.MCPInvocationEntity{}, &types.MCPInvocationDecisionEntity{}))
	fx := &invocationHandlerFixture{
		db:    db,
		mcp:   &fakeHandlerMCPRepo{services: map[string]*types.MCPService{}, metadata: map[string]*types.MCPMetadata{}},
		pol:   &fakeHandlerPolicyRepo{rows: map[string]*types.MCPToolApproval{}},
		oauth: &fakeHandlerOAuthRepo{tokens: map[string]*types.MCPOAuthToken{}},
		exec:  &fakeHandlerExecutor{},
	}
	svc := service.NewMCPInvocationService(
		repository.NewMCPInvocationRepository(db),
		fx.mcp, fx.pol, fx.oauth, fx.exec,
		&config.IntegrationConfig{
			ContextKey: hKeySig, ContextAudience: hAud, ArgsKey: "handler-args-key",
			WriteTimeoutSeconds: 30, ApprovalLeaseSeconds: 900, OAuthTimeoutSeconds: 600, ResultTTLDays: 7,
		})
	h := NewMCPInvocationHandler(svc)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())
	r.Use(func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), hTenant)
		c.Set(types.RequestIDContextKey.String(), "req-test-1")
		ctx := context.WithValue(c.Request.Context(), types.UserIDContextKey, hUser)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	r.POST("/api/v1/integration/mcp/invocations", h.CreateInvocation)
	r.GET("/api/v1/integration/mcp/invocations/:id", h.GetInvocation)
	r.POST("/api/v1/integration/mcp/invocations/:id/cancel", h.CancelInvocation)
	r.POST("/api/v1/integration/mcp/invocations/:id/decision", h.DecideInvocation)
	r.GET("/api/v1/integration/mcp/services", h.ListServices)
	fx.router = r
	return fx
}

// hCtxJWT mints a valid platform invocation context for the fixture caller,
// binding the canonical digest of args. The create surface rejects requests
// without one (BND-150), and each successful create consumes its jti.
func (fx *invocationHandlerFixture) hCtxJWT(t *testing.T, args map[string]any) string {
	t.Helper()
	fx.jti++
	if args == nil {
		args = map[string]any{}
	}
	return signHandlerContext(t, hKeySig, jwt.MapClaims{
		"sub": hUser, "tenant": float64(hTenant), "aud": hAud,
		"jti": fmt.Sprintf("h-jti-auto-%d", fx.jti), "exp": time.Now().Add(time.Minute).Unix(),
		"args_digest": types.MCPInvocationArgsDigest(args),
	})
}

func (fx *invocationHandlerFixture) addService(t *testing.T, id, name string, tools ...*types.MCPTool) {
	t.Helper()
	fx.mcp.services[hKey(strconv.FormatUint(hTenant, 10), id)] = &types.MCPService{
		ID: id, TenantID: hTenant, Name: name, Enabled: true,
		TransportType: types.MCPTransportSSE,
		AuthConfig:    &types.MCPAuthConfig{AuthType: types.MCPAuthNone},
	}
	fx.mcp.metadata[hKey(strconv.FormatUint(hTenant, 10), id, "")] = &types.MCPMetadata{
		TenantID: hTenant, ServiceID: id, Tools: tools,
	}
}

func hReadTool(name string) *types.MCPTool {
	b := true
	return &types.MCPTool{
		Name: name, Description: "read " + name,
		Annotations: &types.MCPToolAnnotations{ReadOnlyHint: &b},
	}
}

func hWriteTool(name string) *types.MCPTool {
	return &types.MCPTool{Name: name, Description: "write " + name}
}

func doInvocationJSON(t *testing.T, fx *invocationHandlerFixture, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	fx.router.ServeHTTP(rec, req)
	return rec
}

func decodeInvocationBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &parsed))
	return parsed
}

// --- handler paths -----------------------------------------------------------

func TestMCPInvocationHandlerCreateSucceeds(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "svc", hReadTool("get_thing"))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "ok"}}}

	rec := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", map[string]any{
		"service_id":         "svc-1",
		"tool_name":          "get_thing",
		"arguments":          map[string]any{"id": "a1"},
		"invocation_context": fx.hCtxJWT(t, map[string]any{"id": "a1"}),
	})
	require.Equal(t, http.StatusOK, rec.Code)
	body := decodeInvocationBody(t, rec)
	require.Equal(t, true, body["success"])
	data, ok := body["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, types.MCPInvocationStatusSucceeded, data["status"])
	require.Contains(t, data, "result")
	require.NotEmpty(t, data["invocation_id"])
}

func TestMCPInvocationHandlerCreateWaitsApproval(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "svc", hWriteTool("update_thing"))

	rec := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", map[string]any{
		"service_id":         "svc-1",
		"tool_name":          "update_thing",
		"invocation_context": fx.hCtxJWT(t, nil),
	})
	require.Equal(t, http.StatusAccepted, rec.Code)
	body := decodeInvocationBody(t, rec)
	require.Equal(t, service.MCPInvocationCodeApprovalRequired, body["code"])
	data := body["data"].(map[string]any)
	require.Equal(t, types.MCPInvocationStatusWaitingApproval, data["status"])
}

func TestMCPInvocationHandlerPolicyDenied(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "svc", hWriteTool("update_thing"))
	signed := signHandlerContext(t, hKeySig, jwt.MapClaims{
		"sub": hUser, "tenant": float64(hTenant), "aud": hAud,
		"jti": "h-jti-1", "exp": time.Now().Add(time.Minute).Unix(),
		"actor_kind": "task", "task_id": "task-1", "device": "dev-1",
		"args_digest": types.MCPInvocationArgsDigest(map[string]any{}),
	})

	rec := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", map[string]any{
		"service_id":         "svc-1",
		"tool_name":          "update_thing",
		"invocation_context": signed,
	})
	require.Equal(t, http.StatusForbidden, rec.Code)
	body := decodeInvocationBody(t, rec)
	errBody, ok := body["error"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, service.MCPInvocationCodePolicyDenied, errBody["code"])
	data, ok := body["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, types.MCPInvocationStatusFailed, data["status"])
	require.Equal(t, types.MCPInvocationErrorPolicyDenied, data["error"])
}

func TestMCPInvocationHandlerContextInvalid(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "svc", hReadTool("get_thing"))
	signed := signHandlerContext(t, "a-different-key", jwt.MapClaims{
		"sub": hUser, "tenant": float64(hTenant), "aud": hAud,
		"jti": "h-jti-2", "exp": time.Now().Add(time.Minute).Unix(),
	})

	rec := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", map[string]any{
		"service_id":         "svc-1",
		"tool_name":          "get_thing",
		"invocation_context": signed,
	})
	require.Equal(t, http.StatusForbidden, rec.Code)
	body := decodeInvocationBody(t, rec)
	errBody := body["error"].(map[string]any)
	require.Equal(t, service.MCPInvocationCodeContextInvalid, errBody["code"])
}

func TestMCPInvocationHandlerValidation(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "svc", hReadTool("get_thing"))

	rec := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", map[string]any{
		"tool_name": "get_thing",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", map[string]any{
		"service_id":         "ghost",
		"tool_name":          "get_thing",
		"invocation_context": fx.hCtxJWT(t, nil),
	})
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMCPInvocationHandlerIdempotentReplay(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "svc", hReadTool("get_thing"))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "ok"}}}
	body := map[string]any{
		"service_id":      "svc-1",
		"tool_name":       "get_thing",
		"idempotency_key": "handler-replay-1",
	}
	// The replay repeats the same context: the idempotency short-circuit runs
	// before context validation, so the single-use jti stays valid for it.
	body["invocation_context"] = fx.hCtxJWT(t, nil)

	first := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", body)
	require.Equal(t, http.StatusOK, first.Code)
	second := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", body)
	require.Equal(t, http.StatusOK, second.Code)
	f1 := decodeInvocationBody(t, first)["data"].(map[string]any)
	f2 := decodeInvocationBody(t, second)["data"].(map[string]any)
	require.Equal(t, f1["invocation_id"], f2["invocation_id"])
	require.Equal(t, 1, fx.exec.callCount())
}

func TestMCPInvocationHandlerDecisionFlow(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "svc", hWriteTool("update_thing"))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "done"}}}

	created := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", map[string]any{
		"service_id":         "svc-1",
		"tool_name":          "update_thing",
		"invocation_context": fx.hCtxJWT(t, nil),
	})
	require.Equal(t, http.StatusAccepted, created.Code)
	inv := decodeInvocationBody(t, created)["data"].(map[string]any)
	id := inv["invocation_id"].(string)

	// Wrong digest → 409 DECISION_CONFLICT.
	conflict := doInvocationJSON(t, fx, http.MethodPost,
		"/api/v1/integration/mcp/invocations/"+id+"/decision", map[string]any{
			"decision":       "approve",
			"args_digest":    "wrong",
			"policy_version": inv["policy_version"],
		})
	require.Equal(t, http.StatusConflict, conflict.Code)
	require.Equal(t, service.MCPInvocationCodeDecisionConflict,
		decodeInvocationBody(t, conflict)["error"].(map[string]any)["code"])

	// Correct decision → 200 and execution.
	approved := doInvocationJSON(t, fx, http.MethodPost,
		"/api/v1/integration/mcp/invocations/"+id+"/decision", map[string]any{
			"decision":       "approve",
			"args_digest":    inv["args_digest"],
			"policy_version": inv["policy_version"],
		})
	require.Equal(t, http.StatusOK, approved.Code)
	payload := decodeInvocationBody(t, approved)["data"].(map[string]any)
	require.Equal(t, types.MCPInvocationStatusSucceeded, payload["invocation"].(map[string]any)["status"])
	decision := payload["decision"].(map[string]any)
	require.Equal(t, "approve", decision["decision"])
	require.Equal(t, hUser, decision["decided_by"])

	// Replay returns the stored decision without re-executing.
	replay := doInvocationJSON(t, fx, http.MethodPost,
		"/api/v1/integration/mcp/invocations/"+id+"/decision", map[string]any{
			"decision":       "approve",
			"args_digest":    inv["args_digest"],
			"policy_version": inv["policy_version"],
		})
	require.Equal(t, http.StatusOK, replay.Code)
	replayPayload := decodeInvocationBody(t, replay)["data"].(map[string]any)
	require.Equal(t, decision["decided_by"], replayPayload["decision"].(map[string]any)["decided_by"])
	require.Equal(t, 1, fx.exec.callCount())
}

func TestMCPInvocationHandlerTaskActorDecisionForbidden(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "svc", hReadTool("get_thing"))
	require.NoError(t, fx.pol.SetRequireApproval(context.Background(), hTenant, "svc-1", "get_thing", true))
	signed := signHandlerContext(t, hKeySig, jwt.MapClaims{
		"sub": hUser, "tenant": float64(hTenant), "aud": hAud,
		"jti": "h-jti-task", "exp": time.Now().Add(time.Minute).Unix(),
		"actor_kind": "task", "task_id": "task-2", "device": "dev-2",
		"args_digest": types.MCPInvocationArgsDigest(map[string]any{}),
	})

	created := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", map[string]any{
		"service_id":         "svc-1",
		"tool_name":          "get_thing",
		"invocation_context": signed,
	})
	require.Equal(t, http.StatusAccepted, created.Code)
	inv := decodeInvocationBody(t, created)["data"].(map[string]any)
	id := inv["invocation_id"].(string)

	rec := doInvocationJSON(t, fx, http.MethodPost,
		"/api/v1/integration/mcp/invocations/"+id+"/decision", map[string]any{
			"decision":       "approve",
			"args_digest":    inv["args_digest"],
			"policy_version": inv["policy_version"],
		})
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, service.MCPInvocationCodeDecisionForbidden,
		decodeInvocationBody(t, rec)["error"].(map[string]any)["code"])
}

func TestMCPInvocationHandlerGetAndOwnership(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "svc", hReadTool("get_thing"))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "ok"}}}

	created := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", map[string]any{
		"service_id":         "svc-1",
		"tool_name":          "get_thing",
		"invocation_context": fx.hCtxJWT(t, nil),
	})
	id := decodeInvocationBody(t, created)["data"].(map[string]any)["invocation_id"].(string)

	got := doInvocationJSON(t, fx, http.MethodGet, "/api/v1/integration/mcp/invocations/"+id, nil)
	require.Equal(t, http.StatusOK, got.Code)
	data := decodeInvocationBody(t, got)["data"].(map[string]any)
	require.Equal(t, types.MCPInvocationStatusSucceeded, data["status"])
	require.Contains(t, data, "result")

	missing := doInvocationJSON(t, fx, http.MethodGet, "/api/v1/integration/mcp/invocations/nope", nil)
	require.Equal(t, http.StatusNotFound, missing.Code)
}

func TestMCPInvocationHandlerCancelFlow(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "svc", hWriteTool("update_thing"))

	created := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", map[string]any{
		"service_id":         "svc-1",
		"tool_name":          "update_thing",
		"invocation_context": fx.hCtxJWT(t, nil),
	})
	id := decodeInvocationBody(t, created)["data"].(map[string]any)["invocation_id"].(string)

	cancelled := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations/"+id+"/cancel", nil)
	require.Equal(t, http.StatusOK, cancelled.Code)
	require.Equal(t, types.MCPInvocationStatusCancelled,
		decodeInvocationBody(t, cancelled)["data"].(map[string]any)["status"])

	// A terminal invocation cannot be cancelled again.
	again := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations/"+id+"/cancel", nil)
	require.Equal(t, http.StatusConflict, again.Code)
	require.Equal(t, service.MCPInvocationCodeDecisionConflict,
		decodeInvocationBody(t, again)["error"].(map[string]any)["code"])
}

func TestMCPInvocationHandlerServicesCatalog(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "visible", hReadTool("get_thing"), hWriteTool("update_thing"))
	fx.addService(t, "svc-2", "hidden", hReadTool("get_thing"))
	require.NoError(t, fx.pol.SetEnabled(context.Background(), hTenant, "svc-1", "update_thing", false))
	fx.mcp.services[hKey(strconv.FormatUint(hTenant, 10), "svc-2")].Enabled = false

	rec := doInvocationJSON(t, fx, http.MethodGet, "/api/v1/integration/mcp/services", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	services := decodeInvocationBody(t, rec)["data"].(map[string]any)["services"].([]any)
	require.Len(t, services, 1)
	entry := services[0].(map[string]any)
	require.Equal(t, "svc-1", entry["service_id"])
	tools := entry["tools"].([]any)
	require.Len(t, tools, 1)
	require.Equal(t, "get_thing", tools[0].(map[string]any)["name"])
	require.Equal(t, "read", tools[0].(map[string]any)["risk"])

	// Task capability view only offers the read tool as well (the write tool
	// is disabled here, so user and task views coincide).
	task := doInvocationJSON(t, fx, http.MethodGet, "/api/v1/integration/mcp/services?actor_kind=task", nil)
	require.Equal(t, http.StatusOK, task.Code)
}

func TestMCPInvocationHandlerReadFailureQuery(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "svc", hReadTool("get_thing"))
	fx.exec.err = mcp.ErrNotConnected // read tool: plain upstream failure

	created := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", map[string]any{
		"service_id":         "svc-1",
		"tool_name":          "get_thing",
		"invocation_context": fx.hCtxJWT(t, nil),
	})
	require.Equal(t, http.StatusOK, created.Code)
	id := decodeInvocationBody(t, created)["data"].(map[string]any)["invocation_id"].(string)

	got := doInvocationJSON(t, fx, http.MethodGet, "/api/v1/integration/mcp/invocations/"+id, nil)
	require.Equal(t, http.StatusOK, got.Code)
	// A retry-safe read failure is a plain upstream_error on a 200.
	require.Equal(t, "upstream_error",
		decodeInvocationBody(t, got)["data"].(map[string]any)["error"])
}

func TestMCPInvocationHandlerContextRequired(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "svc", hReadTool("get_thing"))

	rec := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", map[string]any{
		"service_id": "svc-1",
		"tool_name":  "get_thing",
	})
	require.Equal(t, http.StatusForbidden, rec.Code)
	body := decodeInvocationBody(t, rec)
	errBody := body["error"].(map[string]any)
	require.Equal(t, service.MCPInvocationCodeContextInvalid, errBody["code"])
	// The envelope carries the request id for traceability.
	require.Equal(t, "req-test-1", body["request_id"])
}

func TestMCPInvocationHandlerContextArgsDigestMismatch(t *testing.T) {
	fx := newInvocationHandlerFixture(t)
	fx.addService(t, "svc-1", "svc", hReadTool("get_thing"))

	// The context binds the empty-arguments digest while the call carries
	// arguments: the context cannot be reshaped onto a different call.
	rec := doInvocationJSON(t, fx, http.MethodPost, "/api/v1/integration/mcp/invocations", map[string]any{
		"service_id":         "svc-1",
		"tool_name":          "get_thing",
		"arguments":          map[string]any{"id": "a1"},
		"invocation_context": fx.hCtxJWT(t, nil),
	})
	require.Equal(t, http.StatusForbidden, rec.Code)
	body := decodeInvocationBody(t, rec)
	errBody := body["error"].(map[string]any)
	require.Equal(t, service.MCPInvocationCodeContextInvalid, errBody["code"])
}

func signHandlerContext(t *testing.T, key string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(key))
	require.NoError(t, err)
	return signed
}
