package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// MCP invocation lifecycle statuses (EXT-002). The state machine:
//
//	created → policy_checked → failed(policy_denied)
//	                        → waiting_approval → executing (approve)
//	                                             → failed(denied) (deny)
//	                                             → failed(expired) (lease timeout)
//	                        → waiting_oauth  → policy_checked… (poll, token ready)
//	                                         → failed(oauth_timeout)
//	                        → executing → succeeded
//	                                    → failed(upstream_error)
//	                                    → unknown_outcome (panic / write timeout /
//	                                      connection loss; never auto-retried)
//	created | policy_checked → cancelled            (cancel before send)
//	waiting_approval | waiting_oauth → cancelled
//	executing → cancel_requested → terminal by receipt (actual result recorded)
const (
	MCPInvocationStatusCreated         = "created"
	MCPInvocationStatusPolicyChecked   = "policy_checked"
	MCPInvocationStatusWaitingApproval = "waiting_approval"
	MCPInvocationStatusWaitingOAuth    = "waiting_oauth"
	MCPInvocationStatusExecuting       = "executing"
	MCPInvocationStatusSucceeded       = "succeeded"
	MCPInvocationStatusFailed          = "failed"
	MCPInvocationStatusCancelled       = "cancelled"
	MCPInvocationStatusCancelRequested = "cancel_requested"
	MCPInvocationStatusUnknownOutcome  = "unknown_outcome"
)

// Terminal MCP invocation failure reasons, stored in MCPInvocation.Error.
const (
	MCPInvocationErrorPolicyDenied  = "policy_denied"
	MCPInvocationErrorDenied        = "denied"
	MCPInvocationErrorExpired       = "expired"
	MCPInvocationErrorOAuthTimeout  = "oauth_timeout"
	MCPInvocationErrorUpstreamError = "upstream_error"
	// MCPInvocationErrorArgsKeyRequired marks a wait-state entry refused
	// because no args cipher is configured (BND-151 fail-closed, R1).
	MCPInvocationErrorArgsKeyRequired = "args_key_required"
)

// MCPInvocationActorKind values for MCPInvocation.ActorKind.
const (
	MCPInvocationActorUser = "user"
	MCPInvocationActorTask = "task"
)

// MCPInvocationDecisionApprove / Deny are the only valid decision values.
const (
	MCPInvocationDecisionApprove = "approve"
	MCPInvocationDecisionDeny    = "deny"
)

// MCPInvocationEntity is one request-level MCP tool call (EXT-002). See the
// 000130 migration for column semantics.
type MCPInvocationEntity struct {
	InvocationID   string `gorm:"column:invocation_id;type:varchar(36);primaryKey"`
	TenantID       uint64 `gorm:"index"`
	Subject        string `gorm:"type:varchar(512);not null;default:''"`
	ActorKind      string `gorm:"type:varchar(16);not null;default:'user'"`
	TaskID         string `gorm:"type:varchar(64)"`
	Device         string `gorm:"type:varchar(128)"`
	ServiceID      string `gorm:"type:varchar(36);not null"`
	ToolName       string `gorm:"column:tool_name;type:varchar(512);not null"`
	ArgsDigest     string `gorm:"column:args_digest;type:varchar(64);not null"`
	ArgsEncrypted  []byte `gorm:"column:args_encrypted;type:bytea"`
	IdempotencyKey string `gorm:"type:text"`
	Status         string `gorm:"type:varchar(32);not null;default:'created'"`
	PolicyVersion  string `gorm:"type:varchar(64);not null;default:''"`
	// Risk records the tool risk class at policy-check time so the crash sweep
	// (R2) can pick the settle window per call kind: mutating calls are bounded
	// by WriteTimeout; reads get the much longer ReadSweepTimeout.
	Risk       string          `gorm:"type:varchar(16);not null;default:''"`
	ContextJTI string          `gorm:"column:context_jti;type:varchar(64);not null;default:''"`
	ResultRef  json.RawMessage `gorm:"type:jsonb"`
	Error      string
	SentAt     *time.Time
	FinishedAt *time.Time
	ExpiresAt  time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// TableName pins the table so GORM's pluralizer cannot drift.
func (e *MCPInvocationEntity) TableName() string { return "mcp_invocations" }

// BeforeCreate fills the server-side id when the caller left it empty.
func (e *MCPInvocationEntity) BeforeCreate(tx *gorm.DB) error {
	if e.InvocationID == "" {
		e.InvocationID = uuid.New().String()
	}
	return nil
}

// MCPInvocationDecisionEntity is the single effective decision of one waiting
// invocation (EXT-002). See the 000131 migration.
type MCPInvocationDecisionEntity struct {
	InvocationID  string `gorm:"column:invocation_id;type:varchar(36);primaryKey"`
	TenantID      uint64 `gorm:"index"`
	DecidedBy     string `gorm:"type:varchar(512);not null;default:''"`
	Decision      string `gorm:"type:varchar(16);not null"`
	ArgsDigest    string `gorm:"column:args_digest;type:varchar(64);not null"`
	PolicyVersion string `gorm:"type:varchar(64);not null;default:''"`
	DecidedAt     time.Time
}

// TableName pins the table so GORM's pluralizer cannot drift.
func (e *MCPInvocationDecisionEntity) TableName() string { return "mcp_invocation_decisions" }

// MCPInvocationArgsDigest is the canonical SHA-256 digest of invocation
// arguments. encoding/json marshals map keys in sorted order, which makes the
// digest stable for the same logical argument object.
func MCPInvocationArgsDigest(arguments map[string]any) string {
	canonical, err := json.Marshal(arguments)
	if err != nil {
		canonical = []byte("null")
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
