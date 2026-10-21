package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// SkillReleaseRepository persists immutable skill publication records
// (EXT-001). It is a separate aggregate from TenantSkillRepository so the
// install lifecycle and the publication ledger can evolve independently.
type SkillReleaseRepository interface {
	// GetCatalog scopes a definition lookup to its owning workspace; publish
	// refuses to attest a catalog the caller cannot read.
	GetCatalog(ctx context.Context, tenantID uint64, catalogID string) (*types.TenantSkillCatalogEntity, error)
	// CreateSkillRelease inserts one publication row. Unique (catalog,
	// version), unique bundle digest, and opt-in idempotency key violations
	// surface as ordinary constraint errors; the service reconciles them by
	// re-reading the winning row.
	CreateSkillRelease(ctx context.Context, e *types.SkillReleaseEntity) error
	// GetSkillRelease returns nil (no error) when no release has this id.
	GetSkillRelease(ctx context.Context, id string) (*types.SkillReleaseEntity, error)
	// GetSkillReleaseByIdempotencyKey returns the row a replayed publish key
	// names, or nil when the key was never used.
	GetSkillReleaseByIdempotencyKey(ctx context.Context, key string) (*types.SkillReleaseEntity, error)
	// GetSkillReleaseByCatalogVersion returns the row attesting this catalog
	// version, or nil.
	GetSkillReleaseByCatalogVersion(ctx context.Context, catalogID, version string) (*types.SkillReleaseEntity, error)
	// UpdateSkillReleaseStatus writes the lifecycle transition and, for
	// delisting, the moment it happened. Nothing else on the row is mutable.
	UpdateSkillReleaseStatus(ctx context.Context, id, status string, delistedAt *time.Time) error
	// CountPublishedReleasesByArchiveRef reports how many published (not
	// delisted) releases still name this stored object. DeleteCatalog uses it
	// to refuse dropping bytes a live release still serves.
	CountPublishedReleasesByArchiveRef(ctx context.Context, ref string) (int64, error)
	// ListSkillReleases returns every release row; visibility filtering is the
	// service's job because it spans the JSONB target_tenants list.
	ListSkillReleases(ctx context.Context) ([]*types.SkillReleaseEntity, error)
}

type skillReleaseRepository struct{ db *gorm.DB }

// NewSkillReleaseRepository returns a GORM-backed implementation.
func NewSkillReleaseRepository(db *gorm.DB) SkillReleaseRepository {
	return &skillReleaseRepository{db: db}
}

func (r *skillReleaseRepository) GetCatalog(
	ctx context.Context, tenantID uint64, catalogID string,
) (*types.TenantSkillCatalogEntity, error) {
	var e types.TenantSkillCatalogEntity
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, catalogID).
		First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *skillReleaseRepository) CreateSkillRelease(ctx context.Context, e *types.SkillReleaseEntity) error {
	return r.db.WithContext(ctx).Create(e).Error
}

func (r *skillReleaseRepository) GetSkillRelease(ctx context.Context, id string) (*types.SkillReleaseEntity, error) {
	var e types.SkillReleaseEntity
	err := r.db.WithContext(ctx).
		Where("id = ?", id).
		First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *skillReleaseRepository) GetSkillReleaseByIdempotencyKey(
	ctx context.Context, key string,
) (*types.SkillReleaseEntity, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil
	}
	var e types.SkillReleaseEntity
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

func (r *skillReleaseRepository) GetSkillReleaseByCatalogVersion(
	ctx context.Context, catalogID, version string,
) (*types.SkillReleaseEntity, error) {
	var e types.SkillReleaseEntity
	err := r.db.WithContext(ctx).
		Where("catalog_id = ? AND version = ?", catalogID, version).
		Order("created_at ASC").
		First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *skillReleaseRepository) UpdateSkillReleaseStatus(
	ctx context.Context, id, status string, delistedAt *time.Time,
) error {
	return r.db.WithContext(ctx).
		Model(&types.SkillReleaseEntity{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":      status,
			"delisted_at": delistedAt,
			"updated_at":  time.Now(),
		}).Error
}

func (r *skillReleaseRepository) CountPublishedReleasesByArchiveRef(
	ctx context.Context, ref string,
) (int64, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, nil
	}
	var count int64
	err := r.db.WithContext(ctx).
		Model(&types.SkillReleaseEntity{}).
		Where("archive_ref = ? AND status = ?", ref, types.SkillReleaseStatusPublished).
		Count(&count).Error
	return count, err
}

func (r *skillReleaseRepository) ListSkillReleases(ctx context.Context) ([]*types.SkillReleaseEntity, error) {
	var list []*types.SkillReleaseEntity
	err := r.db.WithContext(ctx).
		Order("created_at ASC").
		Find(&list).Error
	if err != nil {
		return nil, err
	}
	return list, nil
}
