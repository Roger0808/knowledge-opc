package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/approval"
	"github.com/Tencent/WeKnora/internal/agent/policy"
	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/mcp"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// Enumerated string codes for MCP invocation failures (EXT-002 / SPEC
// INT-150). They travel in the standard error envelope's code field, alongside
// the numeric AppError codes the rest of the API uses.
const (
	MCPInvocationCodePolicyDenied      = "MCP_POLICY_DENIED"
	MCPInvocationCodeApprovalRequired  = "MCP_APPROVAL_REQUIRED"
	MCPInvocationCodeContextInvalid    = "CONTEXT_INVALID"
	MCPInvocationCodeDecisionConflict  = "DECISION_CONFLICT"
	MCPInvocationCodeExpired           = "INVOCATION_EXPIRED"
	MCPInvocationCodeDecisionForbidden = "MCP_DECISION_FORBIDDEN"
	MCPInvocationCodeArgsKeyRequired   = "INTEGRATION_ARGS_KEY_REQUIRED"
)

// Sentinel failures of the invocation service. The handler maps them onto the
// enumerated codes above; anything else falls through to the shared error
// middleware.
var (
	ErrMCPInvocationNotFound         = errors.New("mcp invocation not found")
	ErrMCPInvocationForbidden        = errors.New("mcp invocation belongs to another caller")
	ErrMCPInvocationPolicyDenied     = errors.New("mcp invocation denied by policy")
	ErrMCPInvocationContextInvalid   = errors.New("invalid invocation context")
	ErrMCPInvocationDecisionConflict = errors.New("invocation decision conflicts with current arguments or policy")
	ErrMCPInvocationExpired          = errors.New("mcp invocation result has expired")
	ErrMCPInvocationInvalid          = errors.New("invalid mcp invocation")
	// ErrMCPInvocationArgsKeyRequired fails closed when an approval/OAuth wait
	// would be entered without an args cipher: the approved arguments could not
	// be reopened at resume time, so the approved digest would not be what
	// executes (BND-151). Operators provision WEKNORA_INTEGRATION_ARGS_KEY.
	ErrMCPInvocationArgsKeyRequired = errors.New("approval/oauth waits require WEKNORA_INTEGRATION_ARGS_KEY")
)

// MCPInvocationCaller is the authenticated identity behind one invocation.
// ActorKind is user unless a validated platform context proves a task run;
// task actors hold no decision rights.
type MCPInvocationCaller struct {
	TenantID  uint64
	UserID    string
	ActorKind string
	TaskID    string
	Device    string
}

// Subject is the stored owner of the invocation: always the authenticated
// user. An unattended task binding rides in the TaskID / Device columns and
// never changes who owns the row; the actor kind gates decision rights.
func (c MCPInvocationCaller) Subject() string {
	return strings.TrimSpace(c.UserID)
}

// MCPInvocationInput is the create-request payload.
type MCPInvocationInput struct {
	ServiceID      string
	ToolName       string
	Arguments      map[string]any
	IdempotencyKey string
	// ContextJWT is the platform-signed invocation context (EXT-002 BND-150).
	// It is mandatory: the context proves the actor kind, binds the caller,
	// tenant and argument digest, and is single-use.
	ContextJWT string
}

// MCPInvocationRecord is the outward view of one invocation row.
type MCPInvocationRecord struct {
	InvocationID  string          `json:"invocation_id"`
	Status        string          `json:"status"`
	Error         string          `json:"error,omitempty"`
	ServiceID     string          `json:"service_id"`
	ToolName      string          `json:"tool_name"`
	ArgsDigest    string          `json:"args_digest"`
	PolicyVersion string          `json:"policy_version,omitempty"`
	ActorKind     string          `json:"actor_kind"`
	Subject       string          `json:"subject,omitempty"`
	TaskID        string          `json:"task_id,omitempty"`
	Device        string          `json:"device,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	FinishedAt    *time.Time      `json:"finished_at,omitempty"`
	ExpiresAt     time.Time       `json:"expires_at"`
}

// MCPInvocationDecisionView is the outward view of a stored decision.
type MCPInvocationDecisionView struct {
	Decision      string    `json:"decision"`
	DecidedBy     string    `json:"decided_by"`
	ArgsDigest    string    `json:"args_digest"`
	PolicyVersion string    `json:"policy_version"`
	DecidedAt     time.Time `json:"decided_at"`
}

// MCPToolCatalogEntry is one tool of the service directory after policy and
// capability filtering.
type MCPToolCatalogEntry struct {
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	Risk            string `json:"risk"`
	RequireApproval bool   `json:"require_approval"`
}

// MCPServicesCatalogEntry is one visible service of the tenant catalog.
type MCPServicesCatalogEntry struct {
	ServiceID string                `json:"service_id"`
	Name      string                `json:"name"`
	Tools     []MCPToolCatalogEntry `json:"tools"`
}

// MCPInvocationExecutor is how the service reaches the MCP manager. The
// production implementation goes through mcp.MCPManager.GetOrCreateClient, so
// connection reuse and the stdio transport ban stay in force; tests swap in a
// fake to drive the state machine deterministically.
type MCPInvocationExecutor interface {
	CallTool(
		ctx context.Context, service *types.MCPService, toolName string, args map[string]any,
	) (*mcp.CallToolResult, error)
}

type managerMCPInvocationExecutor struct{ manager *mcp.MCPManager }

// NewManagerMCPInvocationExecutor builds the production executor.
func NewManagerMCPInvocationExecutor(manager *mcp.MCPManager) MCPInvocationExecutor {
	return &managerMCPInvocationExecutor{manager: manager}
}

func (e *managerMCPInvocationExecutor) CallTool(
	ctx context.Context, service *types.MCPService, toolName string, args map[string]any,
) (*mcp.CallToolResult, error) {
	client, err := e.manager.GetOrCreateClient(ctx, service)
	if err != nil {
		return nil, err
	}
	return client.CallTool(ctx, toolName, args)
}

// MCPInvocationService runs the EXT-002 request-level invocation lifecycle:
// create → policy check → approval / oauth wait → execute → terminal state,
// with single-effective decisions and honest receipts.
type MCPInvocationService struct {
	repo      repository.MCPInvocationRepository
	mcpRepo   interfaces.MCPServiceRepository
	policySvc interfaces.MCPToolApprovalService
	oauthRepo interfaces.MCPOAuthRepository
	executor  MCPInvocationExecutor
	cfg       *config.IntegrationConfig
	now       func() time.Time

	argsCipher cipher.AEAD

	jtiMu   sync.Mutex
	seenJTI map[string]time.Time

	execMu  sync.Mutex
	execCan map[string]context.CancelFunc
}

// NewMCPInvocationService wires the invocation service.
func NewMCPInvocationService(
	repo repository.MCPInvocationRepository,
	mcpRepo interfaces.MCPServiceRepository,
	policySvc interfaces.MCPToolApprovalService,
	oauthRepo interfaces.MCPOAuthRepository,
	executor MCPInvocationExecutor,
	cfg *config.IntegrationConfig,
) *MCPInvocationService {
	s := &MCPInvocationService{
		repo:      repo,
		mcpRepo:   mcpRepo,
		policySvc: policySvc,
		oauthRepo: oauthRepo,
		executor:  executor,
		cfg:       cfg,
		now:       func() time.Time { return time.Now().UTC() },
		seenJTI:   map[string]time.Time{},
		execCan:   map[string]context.CancelFunc{},
	}
	if cfg != nil {
		if aead, err := newArgsAEAD(strings.TrimSpace(cfg.ArgsKey)); err == nil {
			s.argsCipher = aead
		}
	}
	return s
}

// newArgsAEAD normalizes an operator key (base64/hex 32-byte key, or a raw
// passphrase stretched by SHA-256) into an AES-256-GCM cipher.
func newArgsAEAD(key string) (cipher.AEAD, error) {
	if key == "" {
		return nil, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(key); err == nil && len(decoded) == 32 {
		return newAEAD(decoded)
	}
	if decoded, err := hex.DecodeString(key); err == nil && len(decoded) == 32 {
		return newAEAD(decoded)
	}
	sum := sha256.Sum256([]byte(key))
	return newAEAD(sum[:])
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *MCPInvocationService) sealArgs(args map[string]any) ([]byte, error) {
	plain, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("%w: arguments must be JSON-serializable", ErrMCPInvocationInvalid)
	}
	if s.argsCipher == nil {
		// No key configured: arguments are never persisted in plaintext. Only
		// the canonical digest (args_digest) is stored — a row cannot leak call
		// parameters — and the live call passes its arguments in memory. An
		// approval-gated call resumed after a restart therefore has no stored
		// body to reopen; operators needing that provision
		// WEKNORA_INTEGRATION_ARGS_KEY.
		return nil, nil
	}
	nonce := make([]byte, s.argsCipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.argsCipher.Seal(nonce, nonce, plain, nil), nil
}

func (s *MCPInvocationService) openArgs(sealed []byte) (map[string]any, error) {
	if len(sealed) == 0 {
		return nil, nil
	}
	plain := sealed
	if s.argsCipher != nil {
		size := s.argsCipher.NonceSize()
		if len(sealed) <= size {
			return nil, errors.New("sealed arguments are truncated")
		}
		opened, err := s.argsCipher.Open(nil, sealed[:size], sealed[size:], nil)
		if err != nil {
			return nil, fmt.Errorf("unseal invocation arguments: %w", err)
		}
		plain = opened
	}
	var args map[string]any
	if err := json.Unmarshal(plain, &args); err != nil {
		return nil, fmt.Errorf("decode invocation arguments: %w", err)
	}
	return args, nil
}

// Invoke creates one invocation and drives it through policy to its first
// resting state. Terminal and executing states return synchronously;
// waiting_approval / waiting_oauth return for the client to poll or decide.
func (s *MCPInvocationService) Invoke(
	ctx context.Context, caller MCPInvocationCaller, in MCPInvocationInput,
) (*MCPInvocationRecord, error) {
	serviceID := strings.TrimSpace(in.ServiceID)
	toolName := strings.TrimSpace(in.ToolName)
	if serviceID == "" || toolName == "" {
		return nil, fmt.Errorf("%w: service_id and tool_name are required", ErrMCPInvocationInvalid)
	}
	if in.Arguments == nil {
		in.Arguments = map[string]any{}
	}
	caller = s.normalizeCaller(caller)

	// Idempotent replay returns the original invocation untouched — but only
	// to the tenant and subject that created it; a foreign key reuse is a
	// conflict, not a leak of someone else's row.
	if key := strings.TrimSpace(in.IdempotencyKey); key != "" {
		existing, err := s.repo.GetByIdempotencyKey(ctx, key)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if existing.TenantID != caller.TenantID || existing.Subject != caller.Subject() {
				return nil, fmt.Errorf(
					"%w: idempotency key already used by another workspace", ErrMCPInvocationDecisionConflict)
			}
			return s.view(ctx, existing, true)
		}
	}

	// The platform-signed context is mandatory and authoritative for the
	// actor kind, the task binding and the argument digest; it must match the
	// authenticated user and tenant (BND-150).
	argsDigest := types.MCPInvocationArgsDigest(in.Arguments)
	invCtx, err := s.validateContext(caller, in.ContextJWT, argsDigest)
	if err != nil {
		return nil, err
	}
	if invCtx != nil {
		caller.ActorKind = invCtx.ActorKind
		caller.TaskID = invCtx.TaskID
		caller.Device = invCtx.Device
	}

	svc, err := s.mcpRepo.GetByID(ctx, caller.TenantID, serviceID)
	if err != nil {
		return nil, err
	}
	if svc == nil || !svc.Enabled {
		return nil, ErrMCPInvocationNotFound
	}

	sealed, err := s.sealArgs(in.Arguments)
	if err != nil {
		return nil, err
	}
	now := s.now()
	entity := &types.MCPInvocationEntity{
		TenantID:       caller.TenantID,
		Subject:        caller.Subject(),
		ActorKind:      caller.ActorKind,
		TaskID:         strings.TrimSpace(caller.TaskID),
		Device:         strings.TrimSpace(caller.Device),
		ServiceID:      svc.ID,
		ToolName:       toolName,
		ArgsDigest:     argsDigest,
		ArgsEncrypted:  sealed,
		IdempotencyKey: strings.TrimSpace(in.IdempotencyKey),
		Status:         types.MCPInvocationStatusCreated,
		ExpiresAt:      now.Add(s.cfg.ResultTTL()),
	}
	if invCtx != nil {
		entity.ContextJTI = invCtx.JTI
	}
	if err := s.repo.Create(ctx, entity); err != nil {
		// A unique-violation replay (idempotency key or context jti) returns
		// the already-created row instead of failing — when that row actually
		// belongs to this caller. A foreign winner means a key collision.
		replay, foreign := s.replayOf(ctx, entity)
		if replay != nil {
			return s.view(ctx, replay, true)
		}
		if foreign {
			return nil, fmt.Errorf(
				"%w: idempotency key or context already used by another workspace",
				ErrMCPInvocationDecisionConflict)
		}
		return nil, err
	}
	s.noteJTI(entity.ContextJTI)

	return s.advance(ctx, caller, entity, svc, in.Arguments)
}

// advance runs policy and either executes or parks the invocation in its
// first resting state. args travels in memory: the stored row holds the
// sealed body (or just the digest when no ArgsKey is configured), never
// plaintext.
func (s *MCPInvocationService) advance(
	ctx context.Context, caller MCPInvocationCaller, inv *types.MCPInvocationEntity,
	svc *types.MCPService, args map[string]any,
) (*MCPInvocationRecord, error) {
	tool, verdict, risk, policyVersion, err := s.evaluatePolicy(ctx, caller, svc, inv.ToolName)
	if err != nil {
		return nil, err
	}
	inv.PolicyVersion = policyVersion
	inv.Risk = string(risk)
	if _, err := s.repo.MarkPolicyChecked(ctx, inv.InvocationID, policyVersion, string(risk)); err != nil {
		return nil, err
	}

	switch verdict {
	case policy.Deny:
		reason := types.MCPInvocationErrorPolicyDenied
		finished := s.now()
		if _, err := s.repo.Finish(ctx, inv.InvocationID,
			[]string{types.MCPInvocationStatusCreated, types.MCPInvocationStatusPolicyChecked},
			types.MCPInvocationStatusFailed, &reason, nil, finished); err != nil {
			return nil, err
		}
		inv.Status = types.MCPInvocationStatusFailed
		inv.Error = reason
		inv.FinishedAt = &finished
		rec, _ := s.view(ctx, inv, true)
		return rec, ErrMCPInvocationPolicyDenied
	case policy.Ask:
		if s.argsCipher == nil {
			// R1 修复（BND-151）：无参数加密密钥时无法把已批准的真实参数存到
			// 恢复时点——批准的是真实参数的 digest，恢复执行却拿不到正文。
			// 失败关闭：配置 WEKNORA_INTEGRATION_ARGS_KEY 前不入等待态。
			reason := types.MCPInvocationErrorArgsKeyRequired
			finished := s.now()
			if _, ferr := s.repo.Finish(ctx, inv.InvocationID,
				[]string{types.MCPInvocationStatusCreated, types.MCPInvocationStatusPolicyChecked},
				types.MCPInvocationStatusFailed, &reason, nil, finished); ferr != nil {
				return nil, ferr
			}
			return nil, ErrMCPInvocationArgsKeyRequired
		}
		moved, err := s.repo.TransitionStatus(ctx, inv.InvocationID,
			[]string{types.MCPInvocationStatusPolicyChecked}, types.MCPInvocationStatusWaitingApproval, nil, nil)
		if err != nil {
			return nil, err
		}
		if !moved {
			// A racing transition won; report the winner's state.
			latest, err := s.repo.Get(ctx, inv.InvocationID)
			if err != nil {
				return nil, err
			}
			return s.view(ctx, latest, true)
		}
		inv.Status = types.MCPInvocationStatusWaitingApproval
		return s.view(ctx, inv, true)
	}

	// Allow: an OAuth-gated service without a token for this principal parks
	// in waiting_oauth; a later status poll re-runs this path.
	if svc.AuthConfig.IsOAuth() && !s.hasOAuthToken(ctx, caller, inv.ServiceID) {
		if s.argsCipher == nil {
			// R1 修复：与 waiting_approval 同理，OAuth 恢复路径需要从存储体重开
			// 参数正文；无密钥时失败关闭而不是以空参数恢复执行。
			reason := types.MCPInvocationErrorArgsKeyRequired
			finished := s.now()
			if _, ferr := s.repo.Finish(ctx, inv.InvocationID,
				[]string{types.MCPInvocationStatusCreated, types.MCPInvocationStatusPolicyChecked},
				types.MCPInvocationStatusFailed, &reason, nil, finished); ferr != nil {
				return nil, ferr
			}
			return nil, ErrMCPInvocationArgsKeyRequired
		}
		moved, err := s.repo.TransitionStatus(ctx, inv.InvocationID,
			[]string{types.MCPInvocationStatusPolicyChecked}, types.MCPInvocationStatusWaitingOAuth, nil, nil)
		if err != nil {
			return nil, err
		}
		if !moved {
			latest, err := s.repo.Get(ctx, inv.InvocationID)
			if err != nil {
				return nil, err
			}
			return s.view(ctx, latest, true)
		}
		inv.Status = types.MCPInvocationStatusWaitingOAuth
		return s.view(ctx, inv, true)
	}

	return s.execute(ctx, caller, inv, svc, tool, risk, args)
}

// execute performs the upstream call and lands the terminal state from the
// actual receipt. It re-validates the standing policy immediately before the
// call leaves, as SPEC INT-150 requires. args are the resolved invocation
// arguments: in-memory on the synchronous create path, reopened from the
// sealed body when the call is resumed from storage (approval, OAuth poll).
func (s *MCPInvocationService) execute(
	ctx context.Context, caller MCPInvocationCaller, inv *types.MCPInvocationEntity,
	svc *types.MCPService, tool *types.MCPTool, risk policy.Risk, args map[string]any,
) (*MCPInvocationRecord, error) {
	// Pre-flight re-check: a tool disabled (or policy flipped to deny) after
	// the invocation was created or approved must not run.
	_, verdict, _, _, err := s.evaluatePolicy(ctx, caller, svc, inv.ToolName)
	if err != nil {
		return nil, err
	}
	if verdict == policy.Deny {
		reason := types.MCPInvocationErrorPolicyDenied
		finished := s.now()
		if _, err := s.repo.Finish(ctx, inv.InvocationID,
			[]string{
				types.MCPInvocationStatusCreated,
				types.MCPInvocationStatusPolicyChecked,
				types.MCPInvocationStatusWaitingApproval,
				types.MCPInvocationStatusWaitingOAuth,
			},
			types.MCPInvocationStatusFailed, &reason, nil, finished); err != nil {
			return nil, err
		}
		inv.Status = types.MCPInvocationStatusFailed
		inv.Error = reason
		inv.FinishedAt = &finished
		rec, _ := s.view(ctx, inv, true)
		return rec, ErrMCPInvocationPolicyDenied
	}

	moved, err := s.repo.TransitionStatus(ctx, inv.InvocationID,
		[]string{
			types.MCPInvocationStatusCreated,
			types.MCPInvocationStatusPolicyChecked,
			types.MCPInvocationStatusWaitingApproval,
			types.MCPInvocationStatusWaitingOAuth,
		},
		types.MCPInvocationStatusExecuting, nil, nil)
	if err != nil {
		return nil, err
	}
	if !moved {
		// A racing cancel won; report the winner's state.
		latest, err := s.repo.Get(ctx, inv.InvocationID)
		if err != nil {
			return nil, err
		}
		return s.view(ctx, latest, true)
	}
	inv.Status = types.MCPInvocationStatusExecuting

	execCtx := ctx
	cancel := context.CancelFunc(func() {})
	if risk.Mutates() {
		execCtx, cancel = context.WithTimeout(ctx, s.cfg.WriteTimeout())
	} else {
		execCtx, cancel = context.WithCancel(ctx)
	}
	s.registerCancel(inv.InvocationID, cancel)
	defer s.unregisterCancel(inv.InvocationID)

	sent := s.now()
	s.repo.MarkSent(ctx, inv.InvocationID, sent)
	inv.SentAt = &sent

	var result *mcp.CallToolResult
	var callErr error
	panicked := callGuarded(func() {
		result, callErr = s.executor.CallTool(execCtx, svc, inv.ToolName, args)
	})

	status := types.MCPInvocationStatusSucceeded
	var reason *string
	var receipt []byte
	finished := s.now()
	switch {
	case panicked:
		// A crashed executor wrote no receipt; the outcome is unknowable.
		status = types.MCPInvocationStatusUnknownOutcome
	case callErr != nil:
		switch {
		case mcp.IsToolCallRetrySafe(callErr):
			// Retry-safe means provably never sent (no connection, terminated
			// session, typed authorization rejection): the honest outcome is
			// an ordinary failure, and the caller may try again.
			status = types.MCPInvocationStatusFailed
			reason = strPtr(types.MCPInvocationErrorUpstreamError)
		case errors.Is(callErr, mcp.ErrToolCallOutcomeUnknown) ||
			errors.Is(execCtx.Err(), context.DeadlineExceeded) ||
			errors.Is(execCtx.Err(), context.Canceled):
			// The request left (or may have left) for the upstream: the write
			// window expired, the connection dropped mid-flight or the caller
			// aborted. Never claim failure — and never auto-retry (BND-152).
			status = types.MCPInvocationStatusUnknownOutcome
		case risk.Mutates():
			// Any other mid-flight failure of a mutating call is ambiguous
			// about whether the side effect landed.
			status = types.MCPInvocationStatusUnknownOutcome
		default:
			// A read-only call that failed without a receipt changed nothing.
			status = types.MCPInvocationStatusFailed
			reason = strPtr(types.MCPInvocationErrorUpstreamError)
		}
	case result != nil && result.IsError:
		status = types.MCPInvocationStatusFailed
		reason = strPtr(types.MCPInvocationErrorUpstreamError)
		if encoded, err := json.Marshal(result); err == nil {
			receipt = encoded
		}
	default:
		if result != nil {
			if encoded, err := json.Marshal(result); err == nil {
				receipt = encoded
			}
		}
	}

	// The winner between executing and cancel_requested decides the stored
	// row; cancel therefore never claims a rollback the receipt cannot back.
	from := []string{types.MCPInvocationStatusExecuting, types.MCPInvocationStatusCancelRequested}
	moved, err = s.repo.Finish(ctx, inv.InvocationID, from, status, reason, receipt, finished)
	if err != nil {
		return nil, err
	}
	if !moved {
		// R2: the row was already settled (e.g. crash sweep or a racing read)
		// before this receipt landed. Keep the receipt observable in logs —
		// dropping it silently would hide the real upstream outcome.
		latest, lerr := s.repo.Get(ctx, inv.InvocationID)
		if lerr != nil {
			return nil, lerr
		}
		slog.Warn("mcp invocation: terminal receipt arrived after the row settled; receipt not stored",
			"invocation_id", inv.InvocationID, "attempted_status", status, "current_status", latest.Status)
		return s.view(ctx, latest, true)
	}
	inv.Status = status
	inv.Error = strValue(reason)
	inv.ResultRef = receipt
	inv.FinishedAt = &finished
	return s.view(ctx, inv, true)
}

// Get returns one invocation the caller may see, advancing lazy expiries
// first: waiting_* leases lapse to failed(expired|oauth_timeout), and a
// finished row past expires_at reports INVOCATION_EXPIRED with only its
// status left.
func (s *MCPInvocationService) Get(
	ctx context.Context, tenantID uint64, subject, id string,
) (*MCPInvocationRecord, error) {
	inv, err := s.owned(ctx, tenantID, subject, id)
	if err != nil {
		return nil, err
	}
	if err := s.applyLazyExpiry(ctx, inv); err != nil {
		return nil, err
	}
	// A waiting_oauth invocation advances when a poll observes the principal's
	// token: re-run policy and execute in line (SPEC INT-150).
	if inv.Status == types.MCPInvocationStatusWaitingOAuth {
		if svc, svcErr := s.mcpRepo.GetByID(ctx, tenantID, inv.ServiceID); svcErr == nil && svc != nil &&
			svc.AuthConfig.IsOAuth() && s.hasOAuthToken(ctx, MCPInvocationCaller{TenantID: tenantID, UserID: subject}, inv.ServiceID) {
			caller := MCPInvocationCaller{TenantID: tenantID, UserID: subject, ActorKind: inv.ActorKind}
			if tool, verdict, risk, _, evalErr := s.evaluatePolicy(ctx, caller, svc, inv.ToolName); evalErr == nil && verdict == policy.Allow {
				args, err := s.openArgs(inv.ArgsEncrypted)
				if err != nil {
					return nil, err
				}
				return s.execute(ctx, caller, inv, svc, tool, risk, args)
			}
		}
	}
	if s.terminal(inv.Status) && s.now().After(inv.ExpiresAt) {
		return &MCPInvocationRecord{
			InvocationID: inv.InvocationID,
			Status:       inv.Status,
			ExpiresAt:    inv.ExpiresAt,
		}, ErrMCPInvocationExpired
	}
	return s.view(ctx, inv, false)
}

// Cancel moves an invocation toward cancellation. Pre-send and parked states
// cancel deterministically; an executing call becomes cancel_requested and
// its in-flight context is aborted, the final state settled by the actual
// receipt.
func (s *MCPInvocationService) Cancel(
	ctx context.Context, tenantID uint64, subject, id string,
) (*MCPInvocationRecord, error) {
	inv, err := s.owned(ctx, tenantID, subject, id)
	if err != nil {
		return nil, err
	}
	if err := s.applyLazyExpiry(ctx, inv); err != nil {
		return nil, err
	}
	now := s.now()
	switch inv.Status {
	case types.MCPInvocationStatusCreated, types.MCPInvocationStatusPolicyChecked,
		types.MCPInvocationStatusWaitingApproval, types.MCPInvocationStatusWaitingOAuth:
		moved, err := s.repo.TransitionStatus(ctx, id,
			[]string{
				types.MCPInvocationStatusCreated,
				types.MCPInvocationStatusPolicyChecked,
				types.MCPInvocationStatusWaitingApproval,
				types.MCPInvocationStatusWaitingOAuth,
			},
			types.MCPInvocationStatusCancelled, nil, &now)
		if err != nil {
			return nil, err
		}
		if !moved {
			// Lost a racing transition; re-read and retry the decision.
			return s.Cancel(ctx, tenantID, subject, id)
		}
		inv.Status = types.MCPInvocationStatusCancelled
		inv.FinishedAt = &now
	case types.MCPInvocationStatusExecuting:
		moved, err := s.repo.TransitionStatus(ctx, id,
			[]string{types.MCPInvocationStatusExecuting},
			types.MCPInvocationStatusCancelRequested, nil, nil)
		if err != nil {
			return nil, err
		}
		if !moved {
			return s.Cancel(ctx, tenantID, subject, id)
		}
		inv.Status = types.MCPInvocationStatusCancelRequested
		if cancel := s.takeCancel(id); cancel != nil {
			cancel()
		}
	default:
		return nil, fmt.Errorf("%w: invocation is already %s", ErrMCPInvocationDecisionConflict, inv.Status)
	}
	return s.view(ctx, inv, false)
}

// Decide records the single effective decision of a waiting invocation and,
// on approval, executes it. A repeated submission returns the stored outcome
// without re-executing. Task actors hold no decision rights (BND-151): the
// caller must be an interactive user, and the decision binds the argument
// digest and policy version in force.
func (s *MCPInvocationService) Decide(
	ctx context.Context, tenantID uint64, userID, id, decision, argsDigest, policyVersion string,
) (*MCPInvocationRecord, *MCPInvocationDecisionView, error) {
	inv, err := s.repo.Get(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, nil, err
	}
	if inv == nil || inv.TenantID != tenantID {
		return nil, nil, ErrMCPInvocationNotFound
	}
	// Existence and ownership settle first: a foreign row — whatever its actor
	// kind — is indistinguishable from a missing one (no probing oracle).
	if inv.Subject != strings.TrimSpace(userID) {
		return nil, nil, ErrMCPInvocationNotFound
	}
	// Settle time-driven transitions on the loaded row before anything else
	// decides on it: an expired wait fails(expired) even when the caller never
	// polled Get, and a crashed execution settles as unknown_outcome.
	if err := s.applyLazyExpiry(ctx, inv); err != nil {
		return nil, nil, err
	}
	// Unattended task contexts hold no decision rights (BND-151); this is a
	// deliberate 403, not a hidden 404.
	if inv.ActorKind == types.MCPInvocationActorTask {
		return nil, nil, fmt.Errorf("%w: task contexts cannot submit decisions", ErrMCPInvocationForbidden)
	}
	if existing, err := s.repo.GetDecision(ctx, id); err != nil {
		return nil, nil, err
	} else if existing != nil {
		// Single effective: replaying the decision returns it untouched.
		rec, err := s.view(ctx, inv, false)
		return rec, decisionView(existing), err
	}
	if inv.Status != types.MCPInvocationStatusWaitingApproval {
		return nil, nil, fmt.Errorf(
			"%w: invocation is %s, not waiting for approval", ErrMCPInvocationDecisionConflict, inv.Status)
	}
	decision = strings.ToLower(strings.TrimSpace(decision))
	if decision != types.MCPInvocationDecisionApprove && decision != types.MCPInvocationDecisionDeny {
		return nil, nil, fmt.Errorf("%w: decision must be approve or deny", ErrMCPInvocationInvalid)
	}
	if strings.TrimSpace(argsDigest) != inv.ArgsDigest || strings.TrimSpace(policyVersion) != inv.PolicyVersion {
		return nil, nil, fmt.Errorf(
			"%w: arguments or policy changed since the approval was requested; start a new invocation",
			ErrMCPInvocationDecisionConflict,
		)
	}

	row := &types.MCPInvocationDecisionEntity{
		InvocationID:  id,
		TenantID:      tenantID,
		DecidedBy:     strings.TrimSpace(userID),
		Decision:      decision,
		ArgsDigest:    inv.ArgsDigest,
		PolicyVersion: inv.PolicyVersion,
		DecidedAt:     s.now(),
	}
	if err := s.repo.CreateDecision(ctx, row); err != nil {
		// A racing decision won the primary key; return it instead.
		if existing, err := s.repo.GetDecision(ctx, id); err == nil && existing != nil {
			rec, err := s.view(ctx, inv, false)
			return rec, decisionView(existing), err
		}
		return nil, nil, err
	}

	if decision == types.MCPInvocationDecisionDeny {
		reason := types.MCPInvocationErrorDenied
		finished := s.now()
		if _, err := s.repo.Finish(ctx, id,
			[]string{types.MCPInvocationStatusWaitingApproval},
			types.MCPInvocationStatusFailed, &reason, nil, finished); err != nil {
			return nil, nil, err
		}
		inv.Status = types.MCPInvocationStatusFailed
		inv.Error = reason
		inv.FinishedAt = &finished
		rec, err := s.view(ctx, inv, false)
		return rec, decisionView(row), err
	}

	caller := MCPInvocationCaller{TenantID: tenantID, UserID: userID, ActorKind: types.MCPInvocationActorUser}
	svc, err := s.mcpRepo.GetByID(ctx, tenantID, inv.ServiceID)
	if err != nil {
		return nil, nil, err
	}
	if svc == nil || !svc.Enabled {
		return nil, nil, ErrMCPInvocationNotFound
	}
	tool, verdict, risk, _, err := s.evaluatePolicy(ctx, caller, svc, inv.ToolName)
	if err != nil {
		return nil, nil, err
	}
	if verdict == policy.Deny {
		// The standing policy changed between ask and approve; the bound
		// approval no longer authorizes the call.
		return nil, nil, fmt.Errorf(
			"%w: policy no longer allows this call; start a new invocation", ErrMCPInvocationDecisionConflict)
	}
	args, err := s.openArgs(inv.ArgsEncrypted)
	if err != nil {
		return nil, nil, err
	}
	rec, err := s.execute(ctx, caller, inv, svc, tool, risk, args)
	return rec, decisionView(row), err
}

// Catalog lists the tenant's enabled MCP services with the tools the caller's
// context may actually invoke: the persisted directory ∩ standing policy ∩
// actor capability (unattended task contexts only see what a read-only
// principal may run).
func (s *MCPInvocationService) Catalog(
	ctx context.Context, tenantID uint64, actorKind string,
) ([]MCPServicesCatalogEntry, error) {
	services, err := s.mcpRepo.ListEnabled(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	level := actorLevel(actorKind)
	out := make([]MCPServicesCatalogEntry, 0, len(services))
	for _, svc := range services {
		if svc == nil {
			continue
		}
		tools := s.directoryTools(ctx, tenantID, svc)
		if len(tools) == 0 {
			continue
		}
		entries := make([]MCPToolCatalogEntry, 0, len(tools))
		for _, tool := range tools {
			row, err := s.policySvc.Policy(ctx, tenantID, svc.ID, tool.Name)
			if err != nil {
				return nil, err
			}
			if row != nil && !row.Enabled {
				continue
			}
			risk := approval.ToolRisk(row, approval.DeclaredRisk(tool))
			if !policy.Offered(level, risk) {
				continue
			}
			entries = append(entries, MCPToolCatalogEntry{
				Name:            tool.Name,
				Description:     tool.Description,
				Risk:            string(risk),
				RequireApproval: row != nil && row.RequireApproval,
			})
		}
		if len(entries) == 0 {
			continue
		}
		out = append(out, MCPServicesCatalogEntry{
			ServiceID: svc.ID,
			Name:      svc.Name,
			Tools:     entries,
		})
	}
	return out, nil
}

// evaluatePolicy resolves the standing policy for one tool and decides it at
// the caller actor's level. Interactive users run at "confirm" (mutations
// ask); unattended task contexts run at "read_only" (mutations deny).
func (s *MCPInvocationService) evaluatePolicy(
	ctx context.Context, caller MCPInvocationCaller, svc *types.MCPService, toolName string,
) (*types.MCPTool, policy.Verdict, policy.Risk, string, error) {
	tool := s.directoryTool(ctx, caller.TenantID, svc, toolName)
	if tool == nil {
		return nil, policy.Deny, "", "", fmt.Errorf(
			"%w: tool %q is not in the persisted directory of service %q",
			ErrMCPInvocationInvalid, toolName, svc.Name)
	}
	row, err := s.policySvc.Policy(ctx, caller.TenantID, svc.ID, toolName)
	if err != nil {
		return nil, policy.Deny, "", "", err
	}
	if row != nil && !row.Enabled {
		risk := approval.ToolRisk(row, approval.DeclaredRisk(tool))
		return tool, policy.Deny, risk, policyVersionOf(row, risk), nil
	}
	risk := approval.ToolRisk(row, approval.DeclaredRisk(tool))
	action := policy.Action{Risk: risk, Always: row != nil && row.RequireApproval}
	verdict := policy.Decide(action, policy.Rules{Level: actorLevel(caller.ActorKind)})
	return tool, verdict, risk, policyVersionOf(row, risk), nil
}

// actorLevel maps the actor kind onto the access level the policy decides
// at. A task context is unattended: mutating tools are not offered to it, so
// policy.Deny (not a dangling Ask) is the honest verdict for writes.
func actorLevel(actorKind string) types.AssistantAccessLevel {
	if actorKind == types.MCPInvocationActorTask {
		return types.AssistantAccessReadOnly
	}
	return types.AssistantAccessConfirm
}

func (s *MCPInvocationService) normalizeCaller(caller MCPInvocationCaller) MCPInvocationCaller {
	caller.ActorKind = strings.TrimSpace(caller.ActorKind)
	if caller.ActorKind != types.MCPInvocationActorTask {
		caller.ActorKind = types.MCPInvocationActorUser
	}
	return caller
}

// directoryTools returns the persisted tool directory of a service: the
// tenant-wide snapshot for statically-authenticated services, the
// caller-principal snapshot for OAuth services.
func (s *MCPInvocationService) directoryTools(
	ctx context.Context, tenantID uint64, svc *types.MCPService,
) []*types.MCPTool {
	principal := ""
	if svc.AuthConfig.IsOAuth() {
		p := types.MCPOAuthPrincipalFromContext(ctx)
		if !p.Valid() {
			return nil
		}
		principal = p.StorageID()
	}
	metadataRepo, ok := s.mcpRepo.(interfaces.MCPMetadataRepository)
	if !ok {
		return nil
	}
	snapshot, err := metadataRepo.GetMetadata(ctx, tenantID, svc.ID, principal)
	if err != nil || snapshot == nil {
		return nil
	}
	return snapshot.Tools
}

func (s *MCPInvocationService) directoryTool(
	ctx context.Context, tenantID uint64, svc *types.MCPService, toolName string,
) *types.MCPTool {
	for _, tool := range s.directoryTools(ctx, tenantID, svc) {
		if tool != nil && tool.Name == toolName {
			return tool
		}
	}
	return nil
}

func (s *MCPInvocationService) hasOAuthToken(
	ctx context.Context, caller MCPInvocationCaller, serviceID string,
) bool {
	p := types.MCPOAuthPrincipalFromContext(ctx)
	if !p.Valid() {
		return false
	}
	token, err := s.oauthRepo.GetTokenForPrincipal(ctx, caller.TenantID, p, serviceID)
	return err == nil && token != nil
}

// policyVersionOf stamps which policy inputs authorized the call. A stored
// row versions by id and update time; the absent-row default versions by the
// effective risk, so a decision binds to the policy picture that asked for it
// either way.
func policyVersionOf(row *types.MCPToolApproval, risk policy.Risk) string {
	if row == nil {
		return "default:" + string(risk)
	}
	if row.UpdatedAt.IsZero() {
		return row.ID
	}
	return fmt.Sprintf("%s:%d", row.ID, row.UpdatedAt.Unix())
}

// owned fetches an invocation scoped to the caller's tenant and subject. A
// missing or foreign row is indistinguishable (no enumeration oracle).
func (s *MCPInvocationService) owned(
	ctx context.Context, tenantID uint64, subject, id string,
) (*types.MCPInvocationEntity, error) {
	inv, err := s.repo.Get(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if inv == nil || inv.TenantID != tenantID {
		return nil, ErrMCPInvocationNotFound
	}
	if strings.TrimSpace(subject) != "" && inv.Subject != strings.TrimSpace(subject) {
		return nil, ErrMCPInvocationForbidden
	}
	return inv, nil
}

// applyLazyExpiry settles time-driven transitions without a background
// worker: a waiting_approval row whose lease lapsed fails(expired), a
// waiting_oauth row whose window lapsed fails(oauth_timeout), and an
// executing / cancel_requested row whose sent_at is older than the write
// timeout can no longer produce a receipt — the executor context that owned
// it is dead — so it settles as unknown_outcome (crash sweep). Rows already
// settled are left alone.
func (s *MCPInvocationService) applyLazyExpiry(ctx context.Context, inv *types.MCPInvocationEntity) error {
	now := s.now()
	switch inv.Status {
	case types.MCPInvocationStatusWaitingApproval, types.MCPInvocationStatusWaitingOAuth:
		var lease time.Duration
		var reason string
		if inv.Status == types.MCPInvocationStatusWaitingApproval {
			lease = s.cfg.ApprovalLease()
			reason = types.MCPInvocationErrorExpired
		} else {
			lease = s.cfg.OAuthTimeout()
			reason = types.MCPInvocationErrorOAuthTimeout
		}
		if now.Sub(inv.CreatedAt) < lease {
			return nil
		}
		finished := now
		moved, err := s.repo.Finish(ctx, inv.InvocationID,
			[]string{types.MCPInvocationStatusWaitingApproval, types.MCPInvocationStatusWaitingOAuth},
			types.MCPInvocationStatusFailed, &reason, nil, finished)
		if err != nil {
			return err
		}
		if moved {
			inv.Status = types.MCPInvocationStatusFailed
			inv.Error = reason
			inv.FinishedAt = &finished
		}
		return nil
	case types.MCPInvocationStatusExecuting, types.MCPInvocationStatusCancelRequested:
		// A row still executing long past its settle window means the process
		// driving the call died (or hung beyond any receipt we can still
		// honour). The outcome is genuinely unknowable: settle it as
		// unknown_outcome, never as failed — the side effect may have landed.
		// R2: the window follows the recorded risk class — mutating calls are
		// bounded by WriteTimeout (their exec context carries that deadline);
		// reads carry no deadline, so they only sweep past the much longer
		// ReadSweepTimeout. Legacy rows (empty risk) take the write window.
		window := s.cfg.ReadSweepTimeout()
		if policy.Risk(inv.Risk).Mutates() || inv.Risk == "" {
			window = s.cfg.WriteTimeout()
		}
		if inv.SentAt == nil || now.Sub(*inv.SentAt) < window {
			return nil
		}
		finished := now
		moved, err := s.repo.Finish(ctx, inv.InvocationID,
			[]string{types.MCPInvocationStatusExecuting, types.MCPInvocationStatusCancelRequested},
			types.MCPInvocationStatusUnknownOutcome, nil, nil, finished)
		if err != nil {
			return err
		}
		if moved {
			inv.Status = types.MCPInvocationStatusUnknownOutcome
			inv.FinishedAt = &finished
		}
		return nil
	default:
		return nil
	}
}

func (s *MCPInvocationService) terminal(status string) bool {
	switch status {
	case types.MCPInvocationStatusSucceeded,
		types.MCPInvocationStatusFailed,
		types.MCPInvocationStatusCancelled,
		types.MCPInvocationStatusUnknownOutcome:
		return true
	}
	return false
}

// view projects a row. Results attach only to rows the caller may still
// read: terminal rows past expires_at are status-only.
func (s *MCPInvocationService) view(
	ctx context.Context, inv *types.MCPInvocationEntity, refresh bool,
) (*MCPInvocationRecord, error) {
	if inv == nil {
		return nil, ErrMCPInvocationNotFound
	}
	latest := inv
	if refresh {
		if reloaded, err := s.repo.Get(ctx, inv.InvocationID); err != nil {
			return nil, err
		} else if reloaded != nil {
			latest = reloaded
		}
	}
	rec := &MCPInvocationRecord{
		InvocationID:  latest.InvocationID,
		Status:        latest.Status,
		Error:         latest.Error,
		ServiceID:     latest.ServiceID,
		ToolName:      latest.ToolName,
		ArgsDigest:    latest.ArgsDigest,
		PolicyVersion: latest.PolicyVersion,
		ActorKind:     latest.ActorKind,
		Subject:       latest.Subject,
		TaskID:        latest.TaskID,
		Device:        latest.Device,
		CreatedAt:     latest.CreatedAt,
		SentAt:        latest.SentAt,
		FinishedAt:    latest.FinishedAt,
		ExpiresAt:     latest.ExpiresAt,
	}
	if len(latest.ResultRef) > 0 && s.now().Before(latest.ExpiresAt) {
		rec.Result = json.RawMessage(latest.ResultRef)
	}
	return rec, nil
}

func decisionView(row *types.MCPInvocationDecisionEntity) *MCPInvocationDecisionView {
	if row == nil {
		return nil
	}
	return &MCPInvocationDecisionView{
		Decision:      row.Decision,
		DecidedBy:     row.DecidedBy,
		ArgsDigest:    row.ArgsDigest,
		PolicyVersion: row.PolicyVersion,
		DecidedAt:     row.DecidedAt,
	}
}

// replayOf re-reads the row a create race lost to: by idempotency key when
// set, otherwise by context jti. The second return reports that a winning row
// exists but belongs to another tenant or subject — a key collision the
// caller must see as a conflict, never as someone else's invocation.
func (s *MCPInvocationService) replayOf(
	ctx context.Context, entity *types.MCPInvocationEntity,
) (replay *types.MCPInvocationEntity, foreign bool) {
	match := func(row *types.MCPInvocationEntity) (*types.MCPInvocationEntity, bool) {
		if row == nil {
			return nil, false
		}
		if row.TenantID != entity.TenantID || row.Subject != entity.Subject {
			return nil, true
		}
		return row, false
	}
	if key := strings.TrimSpace(entity.IdempotencyKey); key != "" {
		if row, err := s.repo.GetByIdempotencyKey(ctx, key); err == nil && row != nil {
			return match(row)
		}
	}
	if jti := strings.TrimSpace(entity.ContextJTI); jti != "" {
		if row, err := s.repo.GetByContextJTI(ctx, jti); err == nil && row != nil {
			return match(row)
		}
	}
	return nil, false
}

func (s *MCPInvocationService) noteJTI(jti string) {
	jti = strings.TrimSpace(jti)
	if jti == "" {
		return
	}
	s.jtiMu.Lock()
	defer s.jtiMu.Unlock()
	if len(s.seenJTI) > 4096 {
		cutoff := s.now().Add(-2 * s.cfg.ContextMaxAge())
		for k, at := range s.seenJTI {
			if at.Before(cutoff) {
				delete(s.seenJTI, k)
			}
		}
	}
	s.seenJTI[jti] = s.now()
}

func (s *MCPInvocationService) jtiSeen(jti string) bool {
	s.jtiMu.Lock()
	defer s.jtiMu.Unlock()
	_, ok := s.seenJTI[strings.TrimSpace(jti)]
	return ok
}

func (s *MCPInvocationService) registerCancel(id string, cancel context.CancelFunc) {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	s.execCan[id] = cancel
}

func (s *MCPInvocationService) unregisterCancel(id string) {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	delete(s.execCan, id)
}

func (s *MCPInvocationService) takeCancel(id string) context.CancelFunc {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	cancel := s.execCan[id]
	delete(s.execCan, id)
	return cancel
}

func callGuarded(fn func()) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
		}
	}()
	fn()
	return false
}

func strPtr(s string) *string { return &s }

func strValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
