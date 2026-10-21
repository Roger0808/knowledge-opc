package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// MCPInvocationRepository persists request-level MCP tool invocations and
// their single-effective decisions (EXT-002). It is a separate aggregate from
// MCPToolApprovalRepository: the policy table stores standing rules, this one
// stores the lifecycle of each individual call.
type MCPInvocationRepository interface {
	// Create inserts one invocation row. Constraint violations (idempotency
	// key or context jti replay) surface as ordinary errors; the service
	// reconciles them by re-reading the winning row.
	Create(ctx context.Context, e *types.MCPInvocationEntity) error
	// Get returns the invocation, or (nil, nil) when no row has this id.
	Get(ctx context.Context, id string) (*types.MCPInvocationEntity, error)
	// GetByIdempotencyKey returns the row a replayed key names, or nil.
	GetByIdempotencyKey(ctx context.Context, key string) (*types.MCPInvocationEntity, error)
	// GetByContextJTI returns the row a replayed context jti names, or nil.
	GetByContextJTI(ctx context.Context, jti string) (*types.MCPInvocationEntity, error)
	// TransitionStatus moves a row from one of the expected statuses to the
	// next one (compare-and-swap). It reports false when the row is gone or
	// no longer in an expected status, so concurrent cancel/decision/expiry
	// races resolve to a single winner.
	TransitionStatus(
		ctx context.Context, id string, from []string, to string, errReason *string, finishedAt *time.Time,
	) (bool, error)
	// MarkPolicyChecked transitions created → policy_checked and stamps the
	// policy version that authorized the path, so a later decision can bind
	// to exactly the policy picture that asked for it, plus the tool risk
	// class (R2: the crash sweep picks its settle window from it).
	MarkPolicyChecked(ctx context.Context, id, policyVersion, risk string) (bool, error)
	// Finish writes the terminal state together with the upstream receipt.
	// resultRef may be nil (unknown_outcome / failures without a receipt).
	Finish(
		ctx context.Context, id string, from []string, to string, errReason *string, resultRef []byte, finishedAt time.Time,
	) (bool, error)
	// MarkSent stamps sent_at when the call leaves for the upstream service.
	MarkSent(ctx context.Context, id string, at time.Time) error
	// GetDecision returns the stored decision of an invocation, or (nil, nil).
	GetDecision(ctx context.Context, invocationID string) (*types.MCPInvocationDecisionEntity, error)
	// CreateDecision inserts the single effective decision of an invocation;
	// a concurrent second insert fails on the primary key.
	CreateDecision(ctx context.Context, e *types.MCPInvocationDecisionEntity) error
}

type mcpInvocationRepository struct{ db *gorm.DB }

// NewMCPInvocationRepository returns a GORM-backed implementation.
func NewMCPInvocationRepository(db *gorm.DB) MCPInvocationRepository {
	return &mcpInvocationRepository{db: db}
}

func (r *mcpInvocationRepository) Create(ctx context.Context, e *types.MCPInvocationEntity) error {
	return r.db.WithContext(ctx).Create(e).Error
}

func (r *mcpInvocationRepository) Get(ctx context.Context, id string) (*types.MCPInvocationEntity, error) {
	var e types.MCPInvocationEntity
	err := r.db.WithContext(ctx).
		Where("invocation_id = ?", id).
		First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *mcpInvocationRepository) GetByIdempotencyKey(
	ctx context.Context, key string,
) (*types.MCPInvocationEntity, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil
	}
	var e types.MCPInvocationEntity
	err := r.db.WithContext(ctx).
		Where("idempotency_key = ?", key).
		First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *mcpInvocationRepository) GetByContextJTI(
	ctx context.Context, jti string,
) (*types.MCPInvocationEntity, error) {
	jti = strings.TrimSpace(jti)
	if jti == "" {
		return nil, nil
	}
	var e types.MCPInvocationEntity
	err := r.db.WithContext(ctx).
		Where("context_jti = ?", jti).
		First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *mcpInvocationRepository) MarkPolicyChecked(ctx context.Context, id, policyVersion, risk string) (bool, error) {
	res := r.db.WithContext(ctx).
		Model(&types.MCPInvocationEntity{}).
		Where("invocation_id = ? AND status = ?", id, types.MCPInvocationStatusCreated).
		Updates(map[string]any{
			"status":         types.MCPInvocationStatusPolicyChecked,
			"policy_version": policyVersion,
			"risk":           risk,
			"updated_at":     time.Now(),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func (r *mcpInvocationRepository) TransitionStatus(
	ctx context.Context, id string, from []string, to string, errReason *string, finishedAt *time.Time,
) (bool, error) {
	updates := map[string]any{"status": to, "updated_at": time.Now()}
	if errReason != nil {
		updates["error"] = *errReason
	}
	if finishedAt != nil {
		updates["finished_at"] = *finishedAt
	}
	res := r.db.WithContext(ctx).
		Model(&types.MCPInvocationEntity{}).
		Where("invocation_id = ? AND status IN ?", id, from).
		Updates(updates)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func (r *mcpInvocationRepository) Finish(
	ctx context.Context, id string, from []string, to string, errReason *string, resultRef []byte, finishedAt time.Time,
) (bool, error) {
	updates := map[string]any{
		"status":      to,
		"result_ref":  resultRef,
		"finished_at": finishedAt,
		"updated_at":  time.Now(),
	}
	if errReason != nil {
		updates["error"] = *errReason
	}
	res := r.db.WithContext(ctx).
		Model(&types.MCPInvocationEntity{}).
		Where("invocation_id = ? AND status IN ?", id, from).
		Updates(updates)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func (r *mcpInvocationRepository) MarkSent(ctx context.Context, id string, at time.Time) error {
	return r.db.WithContext(ctx).
		Model(&types.MCPInvocationEntity{}).
		Where("invocation_id = ?", id).
		Update("sent_at", at).Error
}

func (r *mcpInvocationRepository) GetDecision(
	ctx context.Context, invocationID string,
) (*types.MCPInvocationDecisionEntity, error) {
	var e types.MCPInvocationDecisionEntity
	err := r.db.WithContext(ctx).
		Where("invocation_id = ?", invocationID).
		First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *mcpInvocationRepository) CreateDecision(ctx context.Context, e *types.MCPInvocationDecisionEntity) error {
	return r.db.WithContext(ctx).Create(e).Error
}
