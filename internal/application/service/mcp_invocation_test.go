package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/mcp"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// --- fakes ---------------------------------------------------------------

type fakeInvocationMCPRepo struct {
	interfaces.MCPServiceRepository
	services map[string]*types.MCPService // key tenantID\x00id
	metadata map[string]*types.MCPMetadata
}

func invKey(parts ...string) string { return strings.Join(parts, "\x00") }

func (f *fakeInvocationMCPRepo) GetByID(_ context.Context, tenantID uint64, id string) (*types.MCPService, error) {
	if svc, ok := f.services[invKey(strconv.FormatUint(tenantID, 10), id)]; ok {
		return svc, nil
	}
	return nil, nil
}

func (f *fakeInvocationMCPRepo) ListEnabled(_ context.Context, tenantID uint64) ([]*types.MCPService, error) {
	out := []*types.MCPService{}
	for _, svc := range f.services {
		if svc.TenantID == tenantID && svc.Enabled {
			out = append(out, svc)
		}
	}
	return out, nil
}

func (f *fakeInvocationMCPRepo) GetMetadata(
	_ context.Context, tenant uint64, service, principal string,
) (*types.MCPMetadata, error) {
	if md, ok := f.metadata[invKey(strconv.FormatUint(tenant, 10), service, principal)]; ok {
		return md, nil
	}
	return nil, nil
}

func (f *fakeInvocationMCPRepo) ListMetadataSummaries(context.Context, uint64, []string) ([]*types.MCPMetadataSummary, error) {
	return nil, nil
}

func (f *fakeInvocationMCPRepo) SaveMetadata(context.Context, *types.MCPMetadata) error { return nil }

type fakeInvocationPolicyRepo struct {
	mu   sync.Mutex
	rows map[string]*types.MCPToolApproval
}

func policyKey(tenantID uint64, serviceID, tool string) string {
	return invKey(strconv.FormatUint(tenantID, 10), serviceID, tool)
}

func (f *fakeInvocationPolicyRepo) ListByService(_ context.Context, tenantID uint64, serviceID string) ([]*types.MCPToolApproval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*types.MCPToolApproval{}
	prefix := invKey(strconv.FormatUint(tenantID, 10), serviceID) + "\x00"
	for k, row := range f.rows {
		if strings.HasPrefix(k, prefix) {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeInvocationPolicyRepo) SetPolicy(_ context.Context, tenantID uint64, serviceID, toolName string, patch types.MCPToolPolicyPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := policyKey(tenantID, serviceID, toolName)
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
	if patch.Risk != nil {
		row.Risk = *patch.Risk
	}
	row.UpdatedAt = time.Now()
	return nil
}

func (f *fakeInvocationPolicyRepo) SetRequireApproval(ctx context.Context, tenantID uint64, serviceID, toolName string, require bool) error {
	return f.SetPolicy(ctx, tenantID, serviceID, toolName, types.MCPToolPolicyPatch{RequireApproval: &require})
}

func (f *fakeInvocationPolicyRepo) SetEnabled(ctx context.Context, tenantID uint64, serviceID, toolName string, enabled bool) error {
	return f.SetPolicy(ctx, tenantID, serviceID, toolName, types.MCPToolPolicyPatch{Enabled: &enabled})
}

func (f *fakeInvocationPolicyRepo) IsRequired(_ context.Context, tenantID uint64, serviceID, toolName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row := f.rows[policyKey(tenantID, serviceID, toolName)]; row != nil {
		return row.RequireApproval, nil
	}
	return false, nil
}

func (f *fakeInvocationPolicyRepo) IsEnabled(_ context.Context, tenantID uint64, serviceID, toolName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row := f.rows[policyKey(tenantID, serviceID, toolName)]; row != nil {
		return row.Enabled, nil
	}
	return true, nil
}

func (f *fakeInvocationPolicyRepo) Policy(_ context.Context, tenantID uint64, serviceID, toolName string) (*types.MCPToolApproval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[policyKey(tenantID, serviceID, toolName)], nil
}

func (f *fakeInvocationPolicyRepo) ObserveRisk(ctx context.Context, tenantID uint64, serviceID, toolName, risk string) error {
	return f.SetPolicy(ctx, tenantID, serviceID, toolName, types.MCPToolPolicyPatch{Risk: &risk})
}

type fakeInvocationOAuthRepo struct {
	interfaces.MCPOAuthRepository
	mu     sync.Mutex
	tokens map[string]*types.MCPOAuthToken
}

func (f *fakeInvocationOAuthRepo) GetTokenForPrincipal(
	_ context.Context, tenantID uint64, principal types.Principal, serviceID string,
) (*types.MCPOAuthToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[invKey(strconv.FormatUint(tenantID, 10), principal.StorageID(), serviceID)], nil
}

type executorCall struct {
	toolName string
	args     map[string]any
}

type fakeInvocationExecutor struct {
	mu     sync.Mutex
	calls  []executorCall
	fn     func(ctx context.Context, service *types.MCPService, toolName string, args map[string]any) (*mcp.CallToolResult, error)
	result *mcp.CallToolResult
	err    error
	panics bool
}

func (f *fakeInvocationExecutor) CallTool(
	ctx context.Context, service *types.MCPService, toolName string, args map[string]any,
) (result *mcp.CallToolResult, err error) {
	f.mu.Lock()
	f.calls = append(f.calls, executorCall{toolName: toolName, args: args})
	fn := f.fn
	panics := f.panics
	f.mu.Unlock()
	if panics {
		panic("executor crashed")
	}
	if fn != nil {
		return fn(ctx, service, toolName, args)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.result, f.err
}

func (f *fakeInvocationExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// --- fixture --------------------------------------------------------------

const (
	invTestTenant = uint64(7)
	invTestUser   = "user-1"
	invTestKey    = "integration-context-test-key"
	invTestAud    = "opc-invocations"
)

type invocationFixture struct {
	db    *gorm.DB
	repo  repository.MCPInvocationRepository
	mcp   *fakeInvocationMCPRepo
	pol   *fakeInvocationPolicyRepo
	oauth *fakeInvocationOAuthRepo
	exec  *fakeInvocationExecutor
	svc   *MCPInvocationService
	now   time.Time
}

func newInvocationFixture(t *testing.T) *invocationFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:mcp-invocation-"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	// The schema comes from the real SQLite migration chain (000046 covers
	// mcp_invocations + mcp_invocation_decisions), not from a GORM
	// AutoMigrate approximation of it.
	applySQLiteMigration(t, db, "000046_opc_integration.up.sql")
	fx := &invocationFixture{
		db:    db,
		repo:  repository.NewMCPInvocationRepository(db),
		mcp:   &fakeInvocationMCPRepo{services: map[string]*types.MCPService{}, metadata: map[string]*types.MCPMetadata{}},
		pol:   &fakeInvocationPolicyRepo{rows: map[string]*types.MCPToolApproval{}},
		oauth: &fakeInvocationOAuthRepo{tokens: map[string]*types.MCPOAuthToken{}},
		exec:  &fakeInvocationExecutor{},
		now:   time.Now().UTC(),
	}
	fx.svc = NewMCPInvocationService(fx.repo, fx.mcp, fx.pol, fx.oauth, fx.exec, &config.IntegrationConfig{
		ContextKey:           invTestKey,
		ContextAudience:      invTestAud,
		ArgsKey:              "invocation-args-test-key",
		WriteTimeoutSeconds:  30,
		ApprovalLeaseSeconds: 900,
		OAuthTimeoutSeconds:  600,
		ResultTTLDays:        7,
	})
	fx.svc.now = func() time.Time { return fx.now }
	return fx
}

// applySQLiteMigration runs a real migrations/sqlite file against the test
// database so the fixture exercises the shipped schema.
func applySQLiteMigration(t *testing.T, db *gorm.DB, file string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "migrations", "sqlite", file))
	require.NoError(t, err)
	require.NoError(t, db.Exec(string(raw)).Error)
}

// normArgs mirrors Invoke's nil-to-empty normalization, so a context's
// args_digest always matches what the service computes.
func normArgs(args map[string]any) map[string]any {
	if args == nil {
		return map[string]any{}
	}
	return args
}

// addService registers an enabled SSE service with a tenant-wide persisted
// tool directory.
func (fx *invocationFixture) addService(id, name string, tools ...*types.MCPTool) *types.MCPService {
	svc := &types.MCPService{
		ID:            id,
		TenantID:      invTestTenant,
		Name:          name,
		Enabled:       true,
		TransportType: types.MCPTransportSSE,
		AuthConfig:    &types.MCPAuthConfig{AuthType: types.MCPAuthNone},
	}
	fx.mcp.services[invKey(strconv.FormatUint(invTestTenant, 10), id)] = svc
	fx.mcp.metadata[invKey(strconv.FormatUint(invTestTenant, 10), id, "")] = &types.MCPMetadata{
		TenantID:  invTestTenant,
		ServiceID: id,
		Tools:     tools,
	}
	return svc
}

func readOnlyPtr() *bool {
	b := true
	return &b
}

func readTool(name string) *types.MCPTool {
	return &types.MCPTool{
		Name:        name,
		Description: "read-only " + name,
		Annotations: &types.MCPToolAnnotations{ReadOnlyHint: readOnlyPtr()},
	}
}

func writeTool(name string) *types.MCPTool {
	return &types.MCPTool{Name: name, Description: "write " + name}
}

func (fx *invocationFixture) caller() MCPInvocationCaller {
	return MCPInvocationCaller{TenantID: invTestTenant, UserID: invTestUser, ActorKind: types.MCPInvocationActorUser}
}

func (fx *invocationFixture) row(t *testing.T, id string) *types.MCPInvocationEntity {
	t.Helper()
	row, err := fx.repo.Get(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, row)
	return row
}

// signContext mints a platform-signed invocation context for tests.
func signContext(t *testing.T, key string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(key))
	require.NoError(t, err)
	return signed
}

// userContextClaims are the claims of an interactive-user context binding the
// digest of args. The invocation context is mandatory and args-bound, so
// every test invocation carries one.
func userContextClaims(jti string, args map[string]any) jwt.MapClaims {
	return jwt.MapClaims{
		"sub":         invTestUser,
		"tenant":      float64(invTestTenant),
		"aud":         invTestAud,
		"jti":         jti,
		"exp":         time.Now().Add(time.Minute).Unix(),
		"args_digest": types.MCPInvocationArgsDigest(normArgs(args)),
	}
}

// ctxJWT signs a valid interactive-user context for the fixture caller.
func (fx *invocationFixture) ctxJWT(t *testing.T, jti string, args map[string]any) string {
	t.Helper()
	return signContext(t, invTestKey, userContextClaims(jti, args))
}

func taskContextClaims(jti string, exp time.Time, args map[string]any) jwt.MapClaims {
	return jwt.MapClaims{
		"sub":         invTestUser,
		"tenant":      float64(invTestTenant),
		"aud":         invTestAud,
		"jti":         jti,
		"exp":         exp.Unix(),
		"actor_kind":  "task",
		"task_id":     "task-9",
		"device":      "dev-1",
		"args_digest": types.MCPInvocationArgsDigest(normArgs(args)),
	}
}

// --- state machine paths ---------------------------------------------------

func TestMCPInvocationAllowReadToolSucceeds(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "ok"}}}
	args := map[string]any{"id": "a1"}

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		Arguments:  args,
		ContextJWT: fx.ctxJWT(t, "jti-allow-read", args),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusSucceeded, rec.Status)
	require.NotEmpty(t, rec.Result)
	require.Equal(t, 1, fx.exec.callCount())
	require.Equal(t, "a1", fx.exec.calls[0].args["id"])

	// The stored body is sealed: the plaintext JSON does not appear on the row.
	row := fx.row(t, rec.InvocationID)
	require.NotEmpty(t, row.ArgsEncrypted)
	require.NotContains(t, string(row.ArgsEncrypted), "a1")
	require.NotNil(t, row.SentAt)
	require.NotNil(t, row.FinishedAt)
}

func TestMCPInvocationWithoutArgsKeyStoresDigestOnly(t *testing.T) {
	fx := newInvocationFixture(t)
	// Without WEKNORA_INTEGRATION_ARGS_KEY the arguments must never touch the
	// database in plaintext: args_encrypted stays NULL and only the digest is
	// recorded. The live call still receives its arguments in memory.
	fx.svc = NewMCPInvocationService(fx.repo, fx.mcp, fx.pol, fx.oauth, fx.exec, &config.IntegrationConfig{
		ContextKey:           invTestKey,
		ContextAudience:      invTestAud,
		WriteTimeoutSeconds:  30,
		ApprovalLeaseSeconds: 900,
		OAuthTimeoutSeconds:  600,
		ResultTTLDays:        7,
	})
	fx.svc.now = func() time.Time { return fx.now }
	fx.addService("svc-1", "svc", readTool("get_thing"))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "ok"}}}
	args := map[string]any{"id": "a1", "query": "plain-text-marker"}

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		Arguments:  args,
		ContextJWT: fx.ctxJWT(t, "jti-nokey", args),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusSucceeded, rec.Status)
	require.Equal(t, 1, fx.exec.callCount())
	require.Equal(t, "plain-text-marker", fx.exec.calls[0].args["query"])

	row := fx.row(t, rec.InvocationID)
	require.Empty(t, row.ArgsEncrypted)
	require.Equal(t, types.MCPInvocationArgsDigest(args), row.ArgsDigest)
	// No plaintext anywhere on the stored row.
	require.NotContains(t, fmt.Sprintf("%v", row), "plain-text-marker")
}

func TestMCPInvocationWriteToolAsksThenApproveSucceeds(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "done"}}}
	args := map[string]any{"id": "a1"}

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		Arguments:  args,
		ContextJWT: fx.ctxJWT(t, "jti-write-ask", args),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusWaitingApproval, rec.Status)
	require.NotEmpty(t, rec.PolicyVersion)
	require.Equal(t, 0, fx.exec.callCount())

	rec, decision, err := fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
		rec.InvocationID, "approve", rec.ArgsDigest, rec.PolicyVersion)
	require.NoError(t, err)
	require.NotNil(t, decision)
	require.Equal(t, types.MCPInvocationDecisionApprove, decision.Decision)
	require.Equal(t, invTestUser, decision.DecidedBy)
	require.Equal(t, types.MCPInvocationStatusSucceeded, rec.Status)
	require.Equal(t, 1, fx.exec.callCount())
}

func TestMCPInvocationTaskWriteToolDenied(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))
	ctxJWT := signContext(t, invTestKey, taskContextClaims("jti-1", time.Now().Add(time.Minute), nil))

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:      "svc-1",
		ToolName:       "update_thing",
		Arguments:      map[string]any{},
		ContextJWT:     ctxJWT,
		IdempotencyKey: "idem-1",
	})
	require.ErrorIs(t, err, ErrMCPInvocationPolicyDenied)
	require.NotNil(t, rec)
	require.Equal(t, types.MCPInvocationStatusFailed, rec.Status)
	require.Equal(t, types.MCPInvocationErrorPolicyDenied, rec.Error)
	require.Equal(t, "task", rec.ActorKind)
	require.Equal(t, "task-9", rec.TaskID)
	require.Equal(t, "dev-1", rec.Device)
	require.Equal(t, 0, fx.exec.callCount())

	// Replaying the same context jti is rejected even with a fresh key.
	_, err = fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:      "svc-1",
		ToolName:       "update_thing",
		Arguments:      map[string]any{},
		ContextJWT:     ctxJWT,
		IdempotencyKey: "idem-2",
	})
	require.ErrorIs(t, err, ErrMCPInvocationContextInvalid)

	// Replaying the same idempotency key returns the original row.
	replay, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:      "svc-1",
		ToolName:       "update_thing",
		Arguments:      map[string]any{},
		ContextJWT:     signContext(t, invTestKey, taskContextClaims("jti-9", time.Now().Add(time.Minute), nil)),
		IdempotencyKey: "idem-1",
	})
	require.NoError(t, err)
	require.Equal(t, rec.InvocationID, replay.InvocationID)
	require.Equal(t, 0, fx.exec.callCount())
}

func TestMCPInvocationDisabledToolDenied(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	require.NoError(t, fx.pol.SetEnabled(context.Background(), invTestTenant, "svc-1", "get_thing", false))

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		ContextJWT: fx.ctxJWT(t, "jti-disabled-tool", nil),
	})
	require.ErrorIs(t, err, ErrMCPInvocationPolicyDenied)
	require.Equal(t, types.MCPInvocationStatusFailed, rec.Status)
	require.Equal(t, types.MCPInvocationErrorPolicyDenied, rec.Error)
}

func TestMCPInvocationUnknownService(t *testing.T) {
	fx := newInvocationFixture(t)
	_, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "missing",
		ToolName:   "whatever",
		ContextJWT: fx.ctxJWT(t, "jti-unknown-svc", nil),
	})
	require.ErrorIs(t, err, ErrMCPInvocationNotFound)
}

func TestMCPInvocationToolNotInDirectory(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	_, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "ghost_tool",
		ContextJWT: fx.ctxJWT(t, "jti-ghost-tool", nil),
	})
	require.ErrorIs(t, err, ErrMCPInvocationInvalid)
}

func TestMCPInvocationRequireApprovalReadToolAsks(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	require.NoError(t, fx.pol.SetRequireApproval(context.Background(), invTestTenant, "svc-1", "get_thing", true))

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		ContextJWT: fx.ctxJWT(t, "jti-ask-read", nil),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusWaitingApproval, rec.Status)
}

func TestMCPInvocationDecideDenyFails(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-decide-deny", nil),
	})
	require.NoError(t, err)

	rec, decision, err := fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
		rec.InvocationID, "deny", rec.ArgsDigest, rec.PolicyVersion)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationDecisionDeny, decision.Decision)
	require.Equal(t, types.MCPInvocationStatusFailed, rec.Status)
	require.Equal(t, types.MCPInvocationErrorDenied, rec.Error)
	require.Equal(t, 0, fx.exec.callCount())
}

func TestMCPInvocationDecisionBindsArgsDigestAndPolicyVersion(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))
	args := map[string]any{"id": "a1"}

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		Arguments:  args,
		ContextJWT: fx.ctxJWT(t, "jti-bind", args),
	})
	require.NoError(t, err)

	_, _, err = fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
		rec.InvocationID, "approve", "deadbeef", rec.PolicyVersion)
	require.ErrorIs(t, err, ErrMCPInvocationDecisionConflict)

	_, _, err = fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
		rec.InvocationID, "approve", rec.ArgsDigest, "stale-version")
	require.ErrorIs(t, err, ErrMCPInvocationDecisionConflict)

	// The failed attempts recorded nothing.
	require.Nil(t, fx.row(t, rec.InvocationID).FinishedAt)
	decision, err := fx.repo.GetDecision(context.Background(), rec.InvocationID)
	require.NoError(t, err)
	require.Nil(t, decision)
}

func TestMCPInvocationDecisionSingleEffective(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "done"}}}

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-single-effective", nil),
	})
	require.NoError(t, err)

	first, decision, err := fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
		rec.InvocationID, "approve", rec.ArgsDigest, rec.PolicyVersion)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusSucceeded, first.Status)

	// A repeated submission returns the stored decision without re-executing.
	replay, again, err := fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
		rec.InvocationID, "approve", rec.ArgsDigest, rec.PolicyVersion)
	require.NoError(t, err)
	require.Equal(t, decision.Decision, again.Decision)
	require.Equal(t, decision.DecidedAt, again.DecidedAt)
	require.Equal(t, first.Status, replay.Status)
	require.Equal(t, 1, fx.exec.callCount())
}

func TestMCPInvocationTaskActorCannotDecide(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	require.NoError(t, fx.pol.SetRequireApproval(context.Background(), invTestTenant, "svc-1", "get_thing", true))

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		ContextJWT: signContext(t, invTestKey, taskContextClaims("jti-task-1", time.Now().Add(time.Minute), nil)),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusWaitingApproval, rec.Status)
	require.Equal(t, "task", rec.ActorKind)

	_, _, err = fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
		rec.InvocationID, "approve", rec.ArgsDigest, rec.PolicyVersion)
	require.ErrorIs(t, err, ErrMCPInvocationForbidden)
}

func TestMCPInvocationDecideForeignSubjectGets404BeforeActorCheck(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	require.NoError(t, fx.pol.SetRequireApproval(context.Background(), invTestTenant, "svc-1", "get_thing", true))

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		ContextJWT: signContext(t, invTestKey, taskContextClaims("jti-task-foreign", time.Now().Add(time.Minute), nil)),
	})
	require.NoError(t, err)
	require.Equal(t, "task", rec.ActorKind)

	// Existence/ownership settle before the task-actor 403: another subject of
	// the same tenant learns nothing about the row, not even its actor kind.
	_, _, err = fx.svc.Decide(context.Background(), invTestTenant, "someone-else",
		rec.InvocationID, "approve", rec.ArgsDigest, rec.PolicyVersion)
	require.ErrorIs(t, err, ErrMCPInvocationNotFound)
}

func TestMCPInvocationWaitingApprovalExpires(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-lease", nil),
	})
	require.NoError(t, err)

	fx.now = fx.now.Add(16 * time.Minute)
	got, err := fx.svc.Get(context.Background(), invTestTenant, invTestUser, rec.InvocationID)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusFailed, got.Status)
	require.Equal(t, types.MCPInvocationErrorExpired, got.Error)

	_, _, err = fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
		rec.InvocationID, "approve", rec.ArgsDigest, rec.PolicyVersion)
	require.ErrorIs(t, err, ErrMCPInvocationDecisionConflict)
}

func TestMCPInvocationDecideAfterLeaseExpiryWithoutGet(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-lease-decide", nil),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusWaitingApproval, rec.Status)

	// The lease lapses and the caller goes straight to Decide without ever
	// polling Get: the decision path itself must settle the expiry first.
	fx.now = fx.now.Add(16 * time.Minute)
	_, _, err = fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
		rec.InvocationID, "approve", rec.ArgsDigest, rec.PolicyVersion)
	require.ErrorIs(t, err, ErrMCPInvocationDecisionConflict)
	require.Contains(t, err.Error(), types.MCPInvocationStatusFailed)

	row := fx.row(t, rec.InvocationID)
	require.Equal(t, types.MCPInvocationStatusFailed, row.Status)
	require.Equal(t, types.MCPInvocationErrorExpired, row.Error)
	require.Equal(t, 0, fx.exec.callCount())
}

func TestMCPInvocationOAuthWaitsThenPollAdvances(t *testing.T) {
	fx := newInvocationFixture(t)
	svc := fx.addService("svc-oauth", "oauth", readTool("get_thing"))
	svc.AuthConfig = &types.MCPAuthConfig{AuthType: types.MCPAuthOAuth}
	// OAuth directories are per-principal snapshots.
	tenant := strconv.FormatUint(invTestTenant, 10)
	fx.mcp.metadata[invKey(tenant, "svc-oauth", "web_user:"+invTestUser)] =
		fx.mcp.metadata[invKey(tenant, "svc-oauth", "")]
	delete(fx.mcp.metadata, invKey(tenant, "svc-oauth", ""))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "ok"}}}

	ctx := context.WithValue(context.Background(), types.UserIDContextKey, invTestUser)
	rec, err := fx.svc.Invoke(ctx, fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-oauth",
		ToolName:   "get_thing",
		ContextJWT: fx.ctxJWT(t, "jti-oauth-poll", nil),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusWaitingOAuth, rec.Status)
	require.Equal(t, 0, fx.exec.callCount())

	got, err := fx.svc.Get(ctx, invTestTenant, invTestUser, rec.InvocationID)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusWaitingOAuth, got.Status)

	fx.oauth.tokens[invKey(tenant, "web_user:"+invTestUser, "svc-oauth")] = &types.MCPOAuthToken{}
	got, err = fx.svc.Get(ctx, invTestTenant, invTestUser, rec.InvocationID)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusSucceeded, got.Status)
	require.Equal(t, 1, fx.exec.callCount())
}

func TestMCPInvocationOAuthTimeout(t *testing.T) {
	fx := newInvocationFixture(t)
	svc := fx.addService("svc-oauth", "oauth", readTool("get_thing"))
	svc.AuthConfig = &types.MCPAuthConfig{AuthType: types.MCPAuthOAuth}
	tenant := strconv.FormatUint(invTestTenant, 10)
	fx.mcp.metadata[invKey(tenant, "svc-oauth", "web_user:"+invTestUser)] =
		fx.mcp.metadata[invKey(tenant, "svc-oauth", "")]
	delete(fx.mcp.metadata, invKey(tenant, "svc-oauth", ""))

	ctx := context.WithValue(context.Background(), types.UserIDContextKey, invTestUser)
	rec, err := fx.svc.Invoke(ctx, fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-oauth",
		ToolName:   "get_thing",
		ContextJWT: fx.ctxJWT(t, "jti-oauth-timeout", nil),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusWaitingOAuth, rec.Status)

	fx.now = fx.now.Add(11 * time.Minute)
	got, err := fx.svc.Get(ctx, invTestTenant, invTestUser, rec.InvocationID)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusFailed, got.Status)
	require.Equal(t, types.MCPInvocationErrorOAuthTimeout, got.Error)
}

func TestMCPInvocationCancelWaiting(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-cancel-waiting", nil),
	})
	require.NoError(t, err)

	got, err := fx.svc.Cancel(context.Background(), invTestTenant, invTestUser, rec.InvocationID)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusCancelled, got.Status)
	require.Equal(t, 0, fx.exec.callCount())
}

func TestMCPInvocationCancelExecutingSettlesByReceipt(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	// The upstream answers on its own schedule, ignoring the abort signal;
	// the receipt must still win over the cancel.
	fx.exec.fn = func(ctx context.Context, _ *types.MCPService, _ string, _ map[string]any) (*mcp.CallToolResult, error) {
		entered <- struct{}{}
		<-release
		return &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "landed"}}}, nil
	}

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-cancel-receipt", nil),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusWaitingApproval, rec.Status)

	// Approval executes synchronously; run it on a goroutine so the cancel
	// can land while the call is in flight.
	done := make(chan *MCPInvocationRecord, 1)
	go func() {
		approved, _, _ := fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
			rec.InvocationID, "approve", rec.ArgsDigest, rec.PolicyVersion)
		done <- approved
	}()
	<-entered

	got, err := fx.svc.Cancel(context.Background(), invTestTenant, invTestUser, rec.InvocationID)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusCancelRequested, got.Status)

	// The upstream answers after the cancel landed: the receipt wins.
	close(release)
	fin := <-done
	require.Equal(t, types.MCPInvocationStatusSucceeded, fin.Status)
	require.NotEmpty(t, fin.Result)
}

func TestMCPInvocationCancelExecutingWithoutReceiptUnknownOutcome(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))
	entered := make(chan struct{}, 1)
	fx.exec.fn = func(ctx context.Context, _ *types.MCPService, _ string, _ map[string]any) (*mcp.CallToolResult, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-cancel-no-receipt", nil),
	})
	require.NoError(t, err)

	done := make(chan *MCPInvocationRecord, 1)
	go func() {
		approved, _, _ := fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
			rec.InvocationID, "approve", rec.ArgsDigest, rec.PolicyVersion)
		done <- approved
	}()
	<-entered

	got, err := fx.svc.Cancel(context.Background(), invTestTenant, invTestUser, rec.InvocationID)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusCancelRequested, got.Status)

	fin := <-done
	require.Equal(t, types.MCPInvocationStatusUnknownOutcome, fin.Status)
	require.Empty(t, fin.Result)
}

func TestMCPInvocationUnknownOutcomeOnPanic(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	fx.exec.panics = true

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		ContextJWT: fx.ctxJWT(t, "jti-panic", nil),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusUnknownOutcome, rec.Status)

	// The status is queryable and carries the unknown_outcome enum string.
	got, err := fx.svc.Get(context.Background(), invTestTenant, invTestUser, rec.InvocationID)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusUnknownOutcome, got.Status)
}

func TestMCPInvocationWriteOutcomeUnknownNoRetry(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))
	// The call was attempted but the response never established whether the
	// server executed the tool: ambiguous, must settle unknown_outcome.
	fx.exec.err = mcp.ErrToolCallOutcomeUnknown

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-write-unknown", nil),
	})
	// A write tool under a user context asks first; approve drives the
	// failing call.
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusWaitingApproval, rec.Status)

	rec, _, err = fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
		rec.InvocationID, "approve", rec.ArgsDigest, rec.PolicyVersion)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusUnknownOutcome, rec.Status)
	require.Equal(t, 1, fx.exec.callCount()) // no auto re-send
}

func TestMCPInvocationWriteNotConnectedFailsNoRetry(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))
	// ErrNotConnected is retry-safe: provably never sent upstream, so the
	// honest outcome is an ordinary failure, not unknown_outcome.
	fx.exec.err = mcp.ErrNotConnected

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-write-notconn", nil),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusWaitingApproval, rec.Status)

	rec, _, err = fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
		rec.InvocationID, "approve", rec.ArgsDigest, rec.PolicyVersion)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusFailed, rec.Status)
	require.Equal(t, types.MCPInvocationErrorUpstreamError, rec.Error)
	require.Equal(t, 1, fx.exec.callCount()) // no auto re-send
}

func TestMCPInvocationUpstreamErrorFailed(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	fx.exec.err = errors.New("boom")

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		ContextJWT: fx.ctxJWT(t, "jti-upstream-boom", nil),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusFailed, rec.Status)
	require.Equal(t, types.MCPInvocationErrorUpstreamError, rec.Error)
}

func TestMCPInvocationErrorReceiptRecorded(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	fx.exec.result = &mcp.CallToolResult{
		Content: []mcp.ContentItem{{Type: "text", Text: "upstream says no"}},
		IsError: true,
	}

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		ContextJWT: fx.ctxJWT(t, "jti-error-receipt", nil),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusFailed, rec.Status)
	require.Equal(t, types.MCPInvocationErrorUpstreamError, rec.Error)
	require.NotEmpty(t, rec.Result) // the receipt is preserved
}

func TestMCPInvocationIdempotentReplay(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "ok"}}}

	in := MCPInvocationInput{
		ServiceID:      "svc-1",
		ToolName:       "get_thing",
		Arguments:      map[string]any{"id": "a1"},
		IdempotencyKey: "replay-key-1",
	}
	// The replay carries the same context: the idempotency short-circuit runs
	// before context validation, so the single-use jti is not a problem.
	in.ContextJWT = fx.ctxJWT(t, "jti-replay", in.Arguments)
	first, err := fx.svc.Invoke(context.Background(), fx.caller(), in)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusSucceeded, first.Status)

	second, err := fx.svc.Invoke(context.Background(), fx.caller(), in)
	require.NoError(t, err)
	require.Equal(t, first.InvocationID, second.InvocationID)
	require.Equal(t, 1, fx.exec.callCount())
}

func TestMCPInvocationGetOwnershipAndExpiry(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "ok"}}}

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		ContextJWT: fx.ctxJWT(t, "jti-ownership", nil),
	})
	require.NoError(t, err)

	// Another user of the same tenant sees nothing.
	_, err = fx.svc.Get(context.Background(), invTestTenant, "someone-else", rec.InvocationID)
	require.ErrorIs(t, err, ErrMCPInvocationForbidden)
	// Another tenant sees nothing.
	_, err = fx.svc.Get(context.Background(), 999, invTestUser, rec.InvocationID)
	require.ErrorIs(t, err, ErrMCPInvocationNotFound)

	// Before expires_at the result is readable.
	got, err := fx.svc.Get(context.Background(), invTestTenant, invTestUser, rec.InvocationID)
	require.NoError(t, err)
	require.NotEmpty(t, got.Result)

	// After expires_at only the status remains.
	fx.now = fx.now.Add(8 * 24 * time.Hour)
	got, err = fx.svc.Get(context.Background(), invTestTenant, invTestUser, rec.InvocationID)
	require.ErrorIs(t, err, ErrMCPInvocationExpired)
	require.NotNil(t, got)
	require.Equal(t, types.MCPInvocationStatusSucceeded, got.Status)
	require.Empty(t, got.Result)
}

func TestMCPInvocationCancelTerminalConflicts(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "ok"}}}

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		ContextJWT: fx.ctxJWT(t, "jti-cancel-terminal", nil),
	})
	require.NoError(t, err)

	_, err = fx.svc.Cancel(context.Background(), invTestTenant, invTestUser, rec.InvocationID)
	require.ErrorIs(t, err, ErrMCPInvocationDecisionConflict)
}

func TestMCPInvocationCatalogFilters(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "visible", readTool("get_thing"), writeTool("update_thing"), readTool("hidden_tool"))
	require.NoError(t, fx.pol.SetEnabled(context.Background(), invTestTenant, "svc-1", "hidden_tool", false))
	disabled := fx.addService("svc-2", "disabled-svc", readTool("get_thing"))
	disabled.Enabled = false

	ctx := context.Background()
	userCatalog, err := fx.svc.Catalog(ctx, invTestTenant, types.MCPInvocationActorUser)
	require.NoError(t, err)
	require.Len(t, userCatalog, 1)
	require.Equal(t, "svc-1", userCatalog[0].ServiceID)
	names := []string{}
	for _, tool := range userCatalog[0].Tools {
		names = append(names, tool.Name)
	}
	require.ElementsMatch(t, []string{"get_thing", "update_thing"}, names)

	// An unattended task context only sees what a read-only principal may run.
	taskCatalog, err := fx.svc.Catalog(ctx, invTestTenant, types.MCPInvocationActorTask)
	require.NoError(t, err)
	require.Len(t, taskCatalog, 1)
	require.Len(t, taskCatalog[0].Tools, 1)
	require.Equal(t, "get_thing", taskCatalog[0].Tools[0].Name)
	require.Equal(t, "read", taskCatalog[0].Tools[0].Risk)
}

// --- invocation context validation (BND-150) -------------------------------

func TestMCPInvocationContextValidation(t *testing.T) {
	emptyArgsDigest := types.MCPInvocationArgsDigest(map[string]any{})
	validUserClaims := func(jti string, exp time.Time) jwt.MapClaims {
		return jwt.MapClaims{
			"sub": invTestUser, "tenant": float64(invTestTenant), "aud": invTestAud,
			"jti": jti, "exp": exp.Unix(), "args_digest": emptyArgsDigest,
		}
	}
	base := time.Now().Add(time.Minute)

	cases := []struct {
		name      string
		key       string
		claims    jwt.MapClaims
		noContext bool
		wantErr   bool
	}{
		{name: "valid user context", key: invTestKey, claims: validUserClaims("ok-1", base)},
		{name: "valid task context", key: invTestKey, claims: taskContextClaims("ok-2", base, nil)},
		{name: "missing context", wantErr: true, noContext: true},
		{name: "bad signature", wantErr: true, key: "wrong-key", claims: validUserClaims("bad-1", base)},
		{name: "expired", wantErr: true, key: invTestKey, claims: validUserClaims("bad-2", time.Now().Add(-time.Minute))},
		{name: "lifetime over 120s", wantErr: true, key: invTestKey, claims: validUserClaims("bad-3", time.Now().Add(180*time.Second))},
		{name: "wrong tenant", wantErr: true, key: invTestKey, claims: jwt.MapClaims{
			"sub": invTestUser, "tenant": float64(999), "aud": invTestAud,
			"jti": "bad-4", "exp": base.Unix(), "args_digest": emptyArgsDigest,
		}},
		{name: "wrong user", wantErr: true, key: invTestKey, claims: jwt.MapClaims{
			"sub": "intruder", "tenant": float64(invTestTenant), "aud": invTestAud,
			"jti": "bad-5", "exp": base.Unix(), "args_digest": emptyArgsDigest,
		}},
		{name: "args_digest mismatch", wantErr: true, key: invTestKey, claims: jwt.MapClaims{
			"sub": invTestUser, "tenant": float64(invTestTenant), "aud": invTestAud,
			"jti": "bad-digest", "exp": base.Unix(),
			"args_digest": types.MCPInvocationArgsDigest(map[string]any{"other": "args"}),
		}},
		{name: "args_digest missing", wantErr: true, key: invTestKey, claims: jwt.MapClaims{
			"sub": invTestUser, "tenant": float64(invTestTenant), "aud": invTestAud,
			"jti": "bad-digest-2", "exp": base.Unix(),
		}},
		{name: "task context without task_id", wantErr: true, key: invTestKey, claims: jwt.MapClaims{
			"sub": invTestUser, "tenant": float64(invTestTenant), "aud": invTestAud,
			"jti": "bad-6", "exp": base.Unix(), "actor_kind": "task", "device": "dev-1",
			"args_digest": emptyArgsDigest,
		}},
		{name: "wrong audience", wantErr: true, key: invTestKey, claims: jwt.MapClaims{
			"sub": invTestUser, "tenant": float64(invTestTenant), "aud": "elsewhere",
			"jti": "bad-7", "exp": base.Unix(), "args_digest": emptyArgsDigest,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newInvocationFixture(t)
			fx.addService("svc-1", "svc", readTool("get_thing"))
			fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "ok"}}}
			signed := ""
			if !tc.noContext {
				signed = signContext(t, tc.key, tc.claims)
			}

			_, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
				ServiceID:  "svc-1",
				ToolName:   "get_thing",
				ContextJWT: signed,
			})
			if tc.wantErr {
				require.ErrorIs(t, err, ErrMCPInvocationContextInvalid)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestMCPInvocationContextAudienceNotConfigured(t *testing.T) {
	fx := newInvocationFixture(t)
	// Fail closed: with no configured audience, even a well-formed signed
	// context is unacceptable.
	fx.svc.cfg.ContextAudience = ""
	fx.addService("svc-1", "svc", readTool("get_thing"))

	_, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		ContextJWT: fx.ctxJWT(t, "jti-noaud", nil),
	})
	require.ErrorIs(t, err, ErrMCPInvocationContextInvalid)
}

func TestMCPInvocationIdempotencyKeyForeignSubjectConflicts(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "ok"}}}

	first := MCPInvocationInput{
		ServiceID:      "svc-1",
		ToolName:       "get_thing",
		IdempotencyKey: "shared-key",
	}
	first.ContextJWT = fx.ctxJWT(t, "jti-own-key", nil)
	_, err := fx.svc.Invoke(context.Background(), fx.caller(), first)
	require.NoError(t, err)

	// The same key under another subject of the same tenant is a conflict,
	// never a leak of the original row.
	foreignCaller := MCPInvocationCaller{TenantID: invTestTenant, UserID: "user-2", ActorKind: types.MCPInvocationActorUser}
	foreignCtx := signContext(t, invTestKey, jwt.MapClaims{
		"sub": "user-2", "tenant": float64(invTestTenant), "aud": invTestAud,
		"jti": "jti-foreign-key", "exp": time.Now().Add(time.Minute).Unix(),
		"args_digest": types.MCPInvocationArgsDigest(map[string]any{}),
	})
	replay := first
	replay.ContextJWT = foreignCtx
	_, err = fx.svc.Invoke(context.Background(), foreignCaller, replay)
	require.ErrorIs(t, err, ErrMCPInvocationDecisionConflict)
}
func TestMCPInvocationContextReplayRejected(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	fx.exec.result = &mcp.CallToolResult{Content: []mcp.ContentItem{{Type: "text", Text: "ok"}}}
	signed := signContext(t, invTestKey, taskContextClaims("jti-replay", time.Now().Add(time.Minute), nil))

	_, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		ContextJWT: signed,
	})
	require.NoError(t, err)

	// Same jti, fresh idempotency key: in-process dedup rejects it.
	_, err = fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:      "svc-1",
		ToolName:       "get_thing",
		ContextJWT:     signed,
		IdempotencyKey: "fresh-key",
	})
	require.ErrorIs(t, err, ErrMCPInvocationContextInvalid)
}

func TestMCPInvocationContextKeyNotConfigured(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.svc.cfg.ContextKey = ""
	fx.addService("svc-1", "svc", readTool("get_thing"))

	_, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		ContextJWT: signContext(t, invTestKey, taskContextClaims("jti-nocfg", time.Now().Add(time.Minute), nil)),
	})
	require.ErrorIs(t, err, ErrMCPInvocationContextInvalid)
}

func TestMCPInvocationArgsDigestChangesWithArguments(t *testing.T) {
	d1 := types.MCPInvocationArgsDigest(map[string]any{"id": "a1", "n": 1})
	d2 := types.MCPInvocationArgsDigest(map[string]any{"n": 1, "id": "a1"})
	d3 := types.MCPInvocationArgsDigest(map[string]any{"id": "a2", "n": 1})
	require.Equal(t, d1, d2) // canonical key order
	require.NotEqual(t, d1, d3)
	require.Len(t, d1, 64)
}

func TestMCPInvocationPolicyVersionTracksPolicyUpdates(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))

	first, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-pol-v1", nil),
	})
	require.NoError(t, err)
	require.NotEmpty(t, first.PolicyVersion)

	require.NoError(t, fx.pol.SetRequireApproval(context.Background(), invTestTenant, "svc-1", "update_thing", true))
	second, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-pol-v2", nil),
	})
	require.NoError(t, err)
	require.NotEqual(t, first.PolicyVersion, second.PolicyVersion)
}

func TestMCPInvocationApprovalRevalidateBlocksFlipToDeny(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-flip-deny", nil),
	})
	require.NoError(t, err)

	// While waiting, the operator disables the tool: the approval can no
	// longer authorize the call.
	require.NoError(t, fx.pol.SetEnabled(context.Background(), invTestTenant, "svc-1", "update_thing", false))
	_, _, err = fx.svc.Decide(context.Background(), invTestTenant, invTestUser,
		rec.InvocationID, "approve", rec.ArgsDigest, rec.PolicyVersion)
	require.ErrorIs(t, err, ErrMCPInvocationDecisionConflict)
	require.Equal(t, 0, fx.exec.callCount())
}

// --- repository failure propagation & crash sweep ---------------------------

// failingInvocationRepo wraps the real repository and fails one chosen method,
// so the service must propagate the error instead of swallowing it.
type failingInvocationRepo struct {
	repository.MCPInvocationRepository
	failMarkPolicyChecked bool
	failTransition        bool
	failFinish            bool
}

func (r *failingInvocationRepo) MarkPolicyChecked(ctx context.Context, id, policyVersion, risk string) (bool, error) {
	if r.failMarkPolicyChecked {
		return false, errors.New("store unavailable")
	}
	return r.MCPInvocationRepository.MarkPolicyChecked(ctx, id, policyVersion, risk)
}

func (r *failingInvocationRepo) TransitionStatus(
	ctx context.Context, id string, from []string, to string, errReason *string, finishedAt *time.Time,
) (bool, error) {
	if r.failTransition {
		return false, errors.New("store unavailable")
	}
	return r.MCPInvocationRepository.TransitionStatus(ctx, id, from, to, errReason, finishedAt)
}

func (r *failingInvocationRepo) Finish(
	ctx context.Context, id string, from []string, to string, errReason *string, resultRef []byte, finishedAt time.Time,
) (bool, error) {
	if r.failFinish {
		return false, errors.New("store unavailable")
	}
	return r.MCPInvocationRepository.Finish(ctx, id, from, to, errReason, resultRef, finishedAt)
}

func TestMCPInvocationRepositoryErrorsPropagate(t *testing.T) {
	cases := map[string]func(*failingInvocationRepo){
		"mark policy checked": func(r *failingInvocationRepo) { r.failMarkPolicyChecked = true },
		"transition to wait":  func(r *failingInvocationRepo) { r.failTransition = true },
	}
	for name, arm := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newInvocationFixture(t)
			repo := &failingInvocationRepo{MCPInvocationRepository: fx.repo}
			arm(repo)
			fx.svc = NewMCPInvocationService(repo, fx.mcp, fx.pol, fx.oauth, fx.exec, &config.IntegrationConfig{
				ContextKey:           invTestKey,
				ContextAudience:      invTestAud,
				ArgsKey:              "invocation-args-test-key",
				WriteTimeoutSeconds:  30,
				ApprovalLeaseSeconds: 900,
				OAuthTimeoutSeconds:  600,
				ResultTTLDays:        7,
			})
			fx.svc.now = func() time.Time { return fx.now }
			fx.addService("svc-1", "svc", writeTool("update_thing"))

			_, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
				ServiceID:  "svc-1",
				ToolName:   "update_thing",
				ContextJWT: fx.ctxJWT(t, "jti-repo-err", nil),
			})
			require.ErrorContains(t, err, "store unavailable")
			require.Equal(t, 0, fx.exec.callCount())
		})
	}
}

func TestMCPInvocationCrashSweepSettlesStaleExecuting(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-crash-sweep", nil),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusWaitingApproval, rec.Status)

	// Simulate a process that moved the row to executing, stamped sent_at and
	// then died before any receipt could land.
	moved, err := fx.repo.TransitionStatus(context.Background(), rec.InvocationID,
		[]string{types.MCPInvocationStatusWaitingApproval}, types.MCPInvocationStatusExecuting, nil, nil)
	require.NoError(t, err)
	require.True(t, moved)
	require.NoError(t, fx.repo.MarkSent(context.Background(), rec.InvocationID, fx.now.Add(-time.Minute)))

	// The write-timeout window (30s) has passed: the read path settles the
	// orphaned execution as unknown_outcome, never as failed.
	got, err := fx.svc.Get(context.Background(), invTestTenant, invTestUser, rec.InvocationID)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusUnknownOutcome, got.Status)

	row := fx.row(t, rec.InvocationID)
	require.Equal(t, types.MCPInvocationStatusUnknownOutcome, row.Status)
	require.NotNil(t, row.FinishedAt)
}

func TestMCPInvocationCrashSweepLeavesFreshExecutingAlone(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", writeTool("update_thing"))

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		ContextJWT: fx.ctxJWT(t, "jti-crash-fresh", nil),
	})
	require.NoError(t, err)

	moved, err := fx.repo.TransitionStatus(context.Background(), rec.InvocationID,
		[]string{types.MCPInvocationStatusWaitingApproval}, types.MCPInvocationStatusExecuting, nil, nil)
	require.NoError(t, err)
	require.True(t, moved)
	require.NoError(t, fx.repo.MarkSent(context.Background(), rec.InvocationID, fx.now))

	// Inside the write window the call may still be genuinely in flight.
	got, err := fx.svc.Get(context.Background(), invTestTenant, invTestUser, rec.InvocationID)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusExecuting, got.Status)
}

// R1 回归：无 WEKNORA_INTEGRATION_ARGS_KEY 时，需审批的调用失败关闭——
// 批准的是真实参数的 digest，恢复时点拿不到正文就绝不能以空参数执行。
func TestMCPInvocationApprovalFailsClosedWithoutArgsKey(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.svc = NewMCPInvocationService(fx.repo, fx.mcp, fx.pol, fx.oauth, fx.exec, &config.IntegrationConfig{
		ContextKey:           invTestKey,
		ContextAudience:      invTestAud,
		WriteTimeoutSeconds:  30,
		ApprovalLeaseSeconds: 900,
		OAuthTimeoutSeconds:  600,
		ResultTTLDays:        7,
	})
	fx.svc.now = func() time.Time { return fx.now }
	fx.addService("svc-1", "svc", writeTool("update_thing"))
	require.NoError(t, fx.pol.SetRequireApproval(context.Background(), invTestTenant, "svc-1", "update_thing", true))

	args := map[string]any{"id": "a1"}
	_, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "update_thing",
		Arguments:  args,
		ContextJWT: fx.ctxJWT(t, "jti-noargskey-approval", args),
	})
	require.ErrorIs(t, err, ErrMCPInvocationArgsKeyRequired)
	require.Equal(t, 0, fx.exec.callCount())
	var waiting int64
	require.NoError(t, fx.db.Model(&types.MCPInvocationEntity{}).
		Where("status = ?", types.MCPInvocationStatusWaitingApproval).Count(&waiting).Error)
	require.Zero(t, waiting, "no row may park in waiting_approval without an args cipher")
}

// R2 回归：崩溃清扫按风险分窗——读类（无执行截止）过了写窗口不结算，
// 过了读窗口（默认 30min）才结算 unknown_outcome。
func TestMCPInvocationCrashSweepReadWindow(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.addService("svc-1", "svc", readTool("get_thing"))
	require.NoError(t, fx.pol.SetRequireApproval(context.Background(), invTestTenant, "svc-1", "get_thing", true))

	rec, err := fx.svc.Invoke(context.Background(), fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-1",
		ToolName:   "get_thing",
		ContextJWT: fx.ctxJWT(t, "jti-read-sweep", nil),
	})
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusWaitingApproval, rec.Status)

	// 行进入 executing 并打了 sent_at（模拟进程在写前崩溃）。
	moved, err := fx.repo.TransitionStatus(context.Background(), rec.InvocationID,
		[]string{types.MCPInvocationStatusWaitingApproval}, types.MCPInvocationStatusExecuting, nil, nil)
	require.NoError(t, err)
	require.True(t, moved)

	// 31s（过写窗口 30s、远未过读窗口 30min）：慢读不结算。
	require.NoError(t, fx.repo.MarkSent(context.Background(), rec.InvocationID, fx.now.Add(-31*time.Second)))
	got, err := fx.svc.Get(context.Background(), invTestTenant, invTestUser, rec.InvocationID)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusExecuting, got.Status)

	// 31min（过读窗口）：进程显然死了，结算 unknown_outcome。
	require.NoError(t, fx.repo.MarkSent(context.Background(), rec.InvocationID, fx.now.Add(-31*time.Minute)))
	got, err = fx.svc.Get(context.Background(), invTestTenant, invTestUser, rec.InvocationID)
	require.NoError(t, err)
	require.Equal(t, types.MCPInvocationStatusUnknownOutcome, got.Status)
}

// R1 回归（OAuth 对称路径）：无参数密钥时 OAuth 等待同样失败关闭。
func TestMCPInvocationOAuthFailsClosedWithoutArgsKey(t *testing.T) {
	fx := newInvocationFixture(t)
	fx.svc = NewMCPInvocationService(fx.repo, fx.mcp, fx.pol, fx.oauth, fx.exec, &config.IntegrationConfig{
		ContextKey:           invTestKey,
		ContextAudience:      invTestAud,
		WriteTimeoutSeconds:  30,
		ApprovalLeaseSeconds: 900,
		OAuthTimeoutSeconds:  600,
		ResultTTLDays:        7,
	})
	fx.svc.now = func() time.Time { return fx.now }
	svc := fx.addService("svc-oauth", "oauth", readTool("get_thing"))
	svc.AuthConfig = &types.MCPAuthConfig{AuthType: types.MCPAuthOAuth}
	tenant := strconv.FormatUint(invTestTenant, 10)
	fx.mcp.metadata[invKey(tenant, "svc-oauth", "web_user:"+invTestUser)] =
		fx.mcp.metadata[invKey(tenant, "svc-oauth", "")]
	delete(fx.mcp.metadata, invKey(tenant, "svc-oauth", ""))

	ctx := context.WithValue(context.Background(), types.UserIDContextKey, invTestUser)
	_, err := fx.svc.Invoke(ctx, fx.caller(), MCPInvocationInput{
		ServiceID:  "svc-oauth",
		ToolName:   "get_thing",
		ContextJWT: fx.ctxJWT(t, "jti-noargskey-oauth", nil),
	})
	require.ErrorIs(t, err, ErrMCPInvocationArgsKeyRequired)
	require.Equal(t, 0, fx.exec.callCount())
	var waiting int64
	require.NoError(t, fx.db.Model(&types.MCPInvocationEntity{}).
		Where("status = ?", types.MCPInvocationStatusWaitingOAuth).Count(&waiting).Error)
	require.Zero(t, waiting, "no row may park in waiting_oauth without an args cipher")
}
