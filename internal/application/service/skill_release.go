package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Tencent/WeKnora/internal/application/repository"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// EXT-001: skill release publication records. The errors below are the typed
// failures the HTTP layer maps onto its enumerated string codes; everything
// else is an ordinary AppError (not found, forbidden, bad request).
var (
	// ErrSkillReleaseManifestInvalid marks a bundle whose opc-client.json is
	// missing, unparseable, or fails validation. Never persisted: the handler
	// maps it to 422 SKILL_MANIFEST_INVALID.
	ErrSkillReleaseManifestInvalid = errors.New("skill release manifest is invalid")
	// ErrSkillReleaseVersionConflict marks a publish whose (catalog, version)
	// already names different bytes. The handler maps it to 409
	// SKILL_VERSION_CONFLICT.
	ErrSkillReleaseVersionConflict = errors.New("skill release version conflict")
	// ErrSkillReleaseDelisted marks a download of a delisted release. The
	// handler maps it to 403 SKILL_DELISTED.
	ErrSkillReleaseDelisted = errors.New("skill release is delisted")
	// ErrSkillReleaseHashMismatch marks a download whose served bytes no
	// longer hash to the digest recorded at publish time. The handler maps
	// it to 422 SKILL_HASH_MISMATCH.
	ErrSkillReleaseHashMismatch = errors.New("skill release hash mismatch")
)

// opc-client.json well-known values (EXT-001 / SPEC-INT-140).
const (
	// skillReleaseManifestFile is the manifest inside a published bundle.
	skillReleaseManifestFile = "opc-client.json"

	skillReleaseExecModeLocalInstruction = "local_instruction"
	skillReleaseExecModeLocalSandbox     = "local_sandbox"
	skillReleaseExecModeCloudOnly        = "cloud_only"

	skillReleaseClientWork = "work"
	skillReleaseClientCode = "code"
)

// SkillReleaseExecModes lists the exec modes a manifest may declare.
var SkillReleaseExecModes = []string{
	skillReleaseExecModeLocalInstruction,
	skillReleaseExecModeLocalSandbox,
	skillReleaseExecModeCloudOnly,
}

// rawSkillReleaseManifest is the wire form of opc-client.json. Every field
// the spec marks as required is a pointer here so "absent" and "empty" stay
// distinguishable during validation.
type rawSkillReleaseManifest struct {
	SkillID               *string  `json:"skill_id"`
	Version               *string  `json:"version"`
	SHA256                *string  `json:"sha256"`
	ExecMode              *string  `json:"exec_mode"`
	Clients               []string `json:"clients"`
	AllowedToolCategories []string `json:"allowed_tool_categories"`
	Dependencies          []string `json:"dependencies"`
	MinClientProtocol     *string  `json:"min_client_protocol"`
}

// ParseSkillReleaseManifest validates one raw opc-client.json document. A
// missing required field, an unknown exec_mode, or a client outside
// {work, code} all fail; the returned error wraps
// ErrSkillReleaseManifestInvalid so callers can map the whole class without
// matching on message text.
func ParseSkillReleaseManifest(data []byte) (*types.SkillReleaseManifest, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("%w: %s is empty", ErrSkillReleaseManifestInvalid, skillReleaseManifestFile)
	}
	var raw rawSkillReleaseManifest
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, fmt.Errorf("%w: %s is not valid JSON: %v",
			ErrSkillReleaseManifestInvalid, skillReleaseManifestFile, err)
	}
	missing := func(name string) error {
		return fmt.Errorf("%w: %s missing required field %q",
			ErrSkillReleaseManifestInvalid, skillReleaseManifestFile, name)
	}
	if raw.SkillID == nil || strings.TrimSpace(*raw.SkillID) == "" {
		return nil, missing("skill_id")
	}
	if raw.Version == nil || strings.TrimSpace(*raw.Version) == "" {
		return nil, missing("version")
	}
	if raw.SHA256 == nil || strings.TrimSpace(*raw.SHA256) == "" {
		return nil, missing("sha256")
	}
	if raw.ExecMode == nil || strings.TrimSpace(*raw.ExecMode) == "" {
		return nil, missing("exec_mode")
	}
	execMode := strings.TrimSpace(*raw.ExecMode)
	if !slicesContains(SkillReleaseExecModes, execMode) {
		return nil, fmt.Errorf("%w: %q is not an allowed exec_mode (%s)",
			ErrSkillReleaseManifestInvalid, execMode, strings.Join(SkillReleaseExecModes, ", "))
	}
	for _, client := range raw.Clients {
		client = strings.TrimSpace(client)
		if client != skillReleaseClientWork && client != skillReleaseClientCode {
			return nil, fmt.Errorf("%w: client %q is not one of %q, %q",
				ErrSkillReleaseManifestInvalid, client, skillReleaseClientWork, skillReleaseClientCode)
		}
	}
	return &types.SkillReleaseManifest{
		SkillID:               strings.TrimSpace(*raw.SkillID),
		Version:               strings.TrimSpace(*raw.Version),
		SHA256:                strings.TrimSpace(*raw.SHA256),
		ExecMode:              execMode,
		Clients:               raw.Clients,
		AllowedToolCategories: raw.AllowedToolCategories,
		Dependencies:          raw.Dependencies,
		MinClientProtocol:     stringPtrValue(raw.MinClientProtocol),
	}, nil
}

func slicesContains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func stringPtrValue(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

// skillReleaseManifestFromBundle extracts and validates opc-client.json from
// an already-parsed bundle, and checks the digest the manifest attests.
// ParseSkillBundle stays the gatekeeper for the archive as a whole (zip
// limits, SKILL.md, traversal); the digest the manifest must agree with is
// the payload digest — the bundle files re-zipped without opc-client.json.
// A manifest could never carry the digest of bytes that include itself (no
// sha256 fixed point exists), so the attested set is defined as everything
// except the manifest, in the deterministic zipSkillFiles order. The OPC
// packager computes the same value before it writes opc-client.json.
func skillReleaseManifestFromBundle(bundle *SkillBundle) (*types.SkillReleaseManifest, error) {
	if bundle == nil {
		return nil, fmt.Errorf("%w: bundle is empty", ErrSkillReleaseManifestInvalid)
	}
	data, ok := bundle.Files[skillReleaseManifestFile]
	if !ok || len(data) == 0 {
		return nil, fmt.Errorf("%w: bundle has no %s", ErrSkillReleaseManifestInvalid, skillReleaseManifestFile)
	}
	manifest, err := ParseSkillReleaseManifest(data)
	if err != nil {
		return nil, err
	}
	digest, err := skillReleasePayloadDigest(bundle)
	if err != nil {
		return nil, err
	}
	if digest != manifest.SHA256 {
		return nil, fmt.Errorf("%w: %s sha256 %q does not match the bundle payload digest %q",
			ErrSkillReleaseManifestInvalid, skillReleaseManifestFile, manifest.SHA256, digest)
	}
	return manifest, nil
}

// skillReleasePayloadDigest hashes the bundle's files excluding
// opc-client.json, in the deterministic order zipSkillFiles writes.
func skillReleasePayloadDigest(bundle *SkillBundle) (string, error) {
	files := make(map[string][]byte, len(bundle.Files))
	for name, body := range bundle.Files {
		if name == skillReleaseManifestFile {
			continue
		}
		files[name] = body
	}
	if len(files) == 0 {
		return "", fmt.Errorf("%w: bundle holds nothing but %s",
			ErrSkillReleaseManifestInvalid, skillReleaseManifestFile)
	}
	payload, err := zipSkillFiles(files)
	if err != nil {
		return "", err
	}
	return skillArchiveSHA256(payload), nil
}

// SkillReleaseArchive is one opened, hash-verified release archive ready to
// stream to a client. Size and SHA256 are what the client gets as
// Content-Length and X-SHA256.
type SkillReleaseArchive struct {
	Body     io.ReadCloser
	Size     int64
	SHA256   string
	FileName string
}

// SkillReleaseService publishes and serves immutable skill release records
// (EXT-001). It deliberately shares the bundle limits and storage resolver
// with TenantSkillService so a release is exactly "the catalog archive,
// attested".
type SkillReleaseService struct {
	releases repository.SkillReleaseRepository
	resolver interfaces.StorageBackendResolver
	now      func() time.Time
}

// NewSkillReleaseService wires the release service. now is injectable for
// tests; nil means time.Now.
func NewSkillReleaseService(
	releases repository.SkillReleaseRepository,
	resolver interfaces.StorageBackendResolver,
) *SkillReleaseService {
	return &SkillReleaseService{
		releases: releases,
		resolver: resolver,
		now:      time.Now,
	}
}

func (s *SkillReleaseService) clock() func() time.Time {
	if s != nil && s.now != nil {
		return s.now
	}
	return time.Now
}

// SkillReleasePublishInput is one publish request after the HTTP layer has
// parsed it. IdempotencyKey comes from the Idempotency-Key header; the two
// target lists come from the optional JSON body.
type SkillReleasePublishInput struct {
	IdempotencyKey string
	TargetTenants  []uint64
	TargetRoles    []string
}

// SkillReleasePublishResult reports what Publish did: Created is false when
// the call returned an already-existing row (idempotent replay or identical
// re-publish) instead of inserting one.
type SkillReleasePublishResult struct {
	Release *types.SkillReleaseEntity
	Created bool
}

// Publish attests the catalog's current archive bytes into a release row.
// The bytes are read back from the file service and re-hashed server-side;
// the manifest is parsed out of the bundle and validated; the attested bytes
// are copied under an immutable per-release key so a later catalog
// re-register or delete can never change what the release serves; nothing is
// written when any check fails.
func (s *SkillReleaseService) Publish(
	ctx context.Context, tenantID uint64, userID, catalogID string, in SkillReleasePublishInput,
) (*SkillReleasePublishResult, error) {
	catalogID = strings.TrimSpace(catalogID)
	if catalogID == "" {
		return nil, apperrors.NewNotFoundError("skill not found")
	}
	if s.releases == nil {
		return nil, apperrors.NewInternalServerError("skill release store is not configured")
	}
	catalog, err := s.releases.GetCatalog(ctx, tenantID, catalogID)
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, apperrors.NewNotFoundError("skill not found")
	}

	// Idempotent replay: same key returns the original row, whatever the
	// catalog has become since — but only to the workspace that created it.
	// Publishing is an Admin action scoped to the source tenant; a replayed
	// key that names another tenant's release is a conflict, not a leak.
	if key := strings.TrimSpace(in.IdempotencyKey); key != "" {
		existing, err := s.releases.GetSkillReleaseByIdempotencyKey(ctx, key)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if existing.TenantID != tenantID {
				return nil, apperrors.NewConflictError(
					"idempotency key already used by another workspace")
			}
			return &SkillReleasePublishResult{Release: existing, Created: false}, nil
		}
	}

	archive, err := s.catalogArchiveBytes(ctx, tenantID, catalog)
	if err != nil {
		return nil, err
	}
	bundle, err := ParseSkillBundle(archive)
	if err != nil {
		return nil, err
	}
	manifest, err := skillReleaseManifestFromBundle(bundle)
	if err != nil {
		return nil, err
	}

	version := strings.TrimSpace(catalog.Version)
	if version == "" {
		version = manifest.Version
	}

	// Same (catalog, version) is only a conflict when the bytes differ;
	// identical bytes re-published return the row that already attests them.
	if existing, err := s.releases.GetSkillReleaseByCatalogVersion(ctx, catalogID, version); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.BundleSHA256 == bundle.SHA256 {
			return &SkillReleasePublishResult{Release: existing, Created: false}, nil
		}
		return nil, fmt.Errorf("%w: version %q of this skill is already published with different content",
			ErrSkillReleaseVersionConflict, version)
	}

	// Copy the attested bytes under an immutable release key BEFORE recording
	// the row. The catalog object stays under catalog lifecycle management —
	// a re-register overwrites it, DeleteCatalog drops it — while the release
	// keeps serving exactly the bytes it attested from its own key. Release
	// keys are only cleaned up after the audit retention window, never by
	// catalog lifecycle operations.
	releaseID := uuid.NewString()
	releaseRef, err := s.storeReleaseArchive(ctx, tenantID, releaseID, archive)
	if err != nil {
		return nil, err
	}

	now := s.clock()()
	row := &types.SkillReleaseEntity{
		ID:             releaseID,
		TenantID:       tenantID,
		CatalogID:      catalogID,
		Version:        version,
		BundleSHA256:   bundle.SHA256,
		ArchiveRef:     releaseRef,
		Manifest:       *manifest,
		TargetTenants:  uniqueTenantIDs(in.TargetTenants),
		TargetRoles:    uniqueNonEmptyStrings(in.TargetRoles),
		IdempotencyKey: strings.TrimSpace(in.IdempotencyKey),
		Status:         types.SkillReleaseStatusPublished,
		PublishedBy:    strings.TrimSpace(userID),
		PublishedAt:    now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.releases.CreateSkillRelease(ctx, row); err != nil {
		// The copied object must not leak when the row never lands.
		s.deleteReleaseArchiveBestEffort(ctx, tenantID, releaseRef)
		if !isSkillNameConflict(err) {
			return nil, err
		}
		// Lost a create race: whichever row won decides whether this is a
		// replay (same bytes) or a real conflict (different bytes).
		winner, lookupErr := s.releases.GetSkillReleaseByCatalogVersion(ctx, catalogID, version)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if winner == nil {
			return nil, err
		}
		if winner.TenantID != tenantID {
			return nil, apperrors.NewConflictError(
				"this skill version is already published by another workspace")
		}
		if winner.BundleSHA256 == bundle.SHA256 {
			return &SkillReleasePublishResult{Release: winner, Created: false}, nil
		}
		return nil, fmt.Errorf("%w: version %q of this skill is already published with different content",
			ErrSkillReleaseVersionConflict, version)
	}
	return &SkillReleasePublishResult{Release: row, Created: true}, nil
}

// storeReleaseArchive writes the attested archive bytes under the immutable
// per-release key tenant-skills/releases/<release_id>.zip.
func (s *SkillReleaseService) storeReleaseArchive(
	ctx context.Context, tenantID uint64, releaseID string, archive []byte,
) (string, error) {
	fs, err := s.fileServiceForTenant(ctx, tenantID)
	if err != nil {
		return "", err
	}
	ref, err := fs.SaveBytes(ctx, archive, tenantID,
		fmt.Sprintf("tenant-skills/releases/%s.zip", releaseID), false)
	if err != nil {
		return "", fmt.Errorf("store skill release archive: %w", err)
	}
	return ref, nil
}

// deleteReleaseArchiveBestEffort removes a copied release object whose row
// never landed. Cleanup failure only leaves an orphan object; it never
// touches catalog bytes or another release's key.
func (s *SkillReleaseService) deleteReleaseArchiveBestEffort(ctx context.Context, tenantID uint64, ref string) {
	fs, err := s.fileServiceForTenant(ctx, tenantID)
	if err != nil || fs == nil {
		return
	}
	_ = fs.DeleteFile(ctx, ref)
}

// ListReleases returns every release visible to the caller's tenant: rows
// the tenant published, plus rows explicitly targeted at it.
func (s *SkillReleaseService) ListReleases(
	ctx context.Context, tenantID uint64,
) ([]*types.SkillReleaseEntity, error) {
	rows, err := s.releases.ListSkillReleases(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*types.SkillReleaseEntity, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		if releaseVisibleTo(row, tenantID) {
			out = append(out, row)
		}
	}
	return out, nil
}

// GetRelease returns one release the caller's tenant may see. Rows that are
// not visible answer 404, not 403, so membership cannot be probed by id.
func (s *SkillReleaseService) GetRelease(
	ctx context.Context, tenantID uint64, releaseID string,
) (*types.SkillReleaseEntity, error) {
	row, err := s.visibleRelease(ctx, tenantID, releaseID)
	if err != nil {
		return nil, err
	}
	return row, nil
}

// OpenArchive streams the release archive to a caller whose tenant can see
// the row and whose role the row's target_roles admit. Bytes are re-hashed
// as they are read; a mismatch against the recorded digest refuses the
// download rather than serving bytes the release does not attest.
func (s *SkillReleaseService) OpenArchive(
	ctx context.Context, tenantID uint64, role types.TenantRole, releaseID string,
) (*SkillReleaseArchive, error) {
	row, err := s.visibleRelease(ctx, tenantID, releaseID)
	if err != nil {
		return nil, err
	}
	if row.Status == types.SkillReleaseStatusDelisted {
		return nil, ErrSkillReleaseDelisted
	}
	if len(row.TargetRoles) > 0 && !slicesContains(row.TargetRoles, string(role)) {
		return nil, apperrors.NewForbiddenError(
			"your role in this workspace cannot download this skill release")
	}
	ref := strings.TrimSpace(row.ArchiveRef)
	if ref == "" {
		return nil, apperrors.NewNotFoundError("skill release archive is not available")
	}
	// The bytes live in the PUBLISHING workspace's storage: a target-tenant
	// download must resolve the file service of the release's source tenant,
	// not of the caller.
	fs, err := s.fileServiceForTenant(ctx, row.TenantID)
	if err != nil {
		return nil, err
	}
	reader, err := fs.GetFile(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("download skill release archive: %w", err)
	}
	defer func() { _ = reader.Close() }()
	archive, err := io.ReadAll(io.LimitReader(reader, maxSkillBundleTotalBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read skill release archive: %w", err)
	}
	if len(archive) > maxSkillBundleTotalBytes {
		return nil, apperrors.NewInternalServerError("skill release archive exceeds the size limit")
	}
	if got := skillArchiveSHA256(archive); got != row.BundleSHA256 {
		return nil, fmt.Errorf("%w: archive now hashes to %s, release %s records %s",
			ErrSkillReleaseHashMismatch, got, row.ID, row.BundleSHA256)
	}
	return &SkillReleaseArchive{
		Body:     io.NopCloser(bytes.NewReader(archive)),
		Size:     int64(len(archive)),
		SHA256:   row.BundleSHA256,
		FileName: skillReleaseArchiveFileName(row),
	}, nil
}

// Delist stops new downloads of one release. Only the source workspace may
// delist its own publication; other workspaces get 404 so they cannot even
// probe the row. Already-delisted rows succeed idempotently.
func (s *SkillReleaseService) Delist(
	ctx context.Context, tenantID uint64, releaseID string,
) (*types.SkillReleaseEntity, error) {
	row, err := s.releases.GetSkillRelease(ctx, strings.TrimSpace(releaseID))
	if err != nil {
		return nil, err
	}
	if row == nil || row.TenantID != tenantID {
		return nil, apperrors.NewNotFoundError("skill release not found")
	}
	if row.Status == types.SkillReleaseStatusDelisted {
		return row, nil
	}
	now := s.clock()()
	if err := s.releases.UpdateSkillReleaseStatus(ctx, row.ID, types.SkillReleaseStatusDelisted, &now); err != nil {
		return nil, err
	}
	row.Status = types.SkillReleaseStatusDelisted
	row.DelistedAt = &now
	row.UpdatedAt = now
	return row, nil
}

// visibleRelease loads one row and checks tenant visibility.
func (s *SkillReleaseService) visibleRelease(
	ctx context.Context, tenantID uint64, releaseID string,
) (*types.SkillReleaseEntity, error) {
	row, err := s.releases.GetSkillRelease(ctx, strings.TrimSpace(releaseID))
	if err != nil {
		return nil, err
	}
	if row == nil || !releaseVisibleTo(row, tenantID) {
		return nil, apperrors.NewNotFoundError("skill release not found")
	}
	return row, nil
}

func releaseVisibleTo(row *types.SkillReleaseEntity, tenantID uint64) bool {
	if row == nil {
		return false
	}
	if row.TenantID == tenantID {
		return true
	}
	for _, id := range row.TargetTenants {
		if id == tenantID {
			return true
		}
	}
	return false
}

// catalogArchiveBytes reads back the stored catalog archive through the same
// file service the catalog itself uses, bounded by the shared bundle cap.
func (s *SkillReleaseService) catalogArchiveBytes(
	ctx context.Context, tenantID uint64, catalog *types.TenantSkillCatalogEntity,
) ([]byte, error) {
	ref := strings.TrimSpace(catalog.BundleRef)
	if ref == "" {
		return nil, apperrors.NewBadRequestError(
			"this skill has no stored archive to publish; re-register it from the original bundle")
	}
	fs, err := s.fileServiceForTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	reader, err := fs.GetFile(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("read skill archive: %w", err)
	}
	defer func() { _ = reader.Close() }()
	archive, err := io.ReadAll(io.LimitReader(reader, maxSkillBundleTotalBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read skill archive: %w", err)
	}
	if len(archive) > maxSkillBundleTotalBytes {
		return nil, fmt.Errorf("%w: archive is too large", ErrSkillBundleInvalid)
	}
	if len(archive) == 0 {
		return nil, apperrors.NewBadRequestError("stored skill archive is empty")
	}
	return archive, nil
}

func (s *SkillReleaseService) fileServiceForTenant(
	ctx context.Context, tenantID uint64,
) (interfaces.FileService, error) {
	if s.resolver == nil {
		return nil, errors.New("storage resolver is not configured")
	}
	fs, _, err := s.resolver.ResolveFileService(ctx, skillReleaseTenantForStorage(ctx, tenantID), "", "", "")
	if err != nil {
		return nil, err
	}
	if fs == nil {
		return nil, errors.New("file service is not configured")
	}
	return fs, nil
}

func skillReleaseTenantForStorage(ctx context.Context, tenantID uint64) *types.Tenant {
	if info, ok := types.TenantInfoFromContext(ctx); ok && info.ID == tenantID {
		return info
	}
	return &types.Tenant{ID: tenantID}
}

func skillReleaseArchiveFileName(row *types.SkillReleaseEntity) string {
	name := "skill"
	if row != nil {
		if m := row.Manifest; strings.TrimSpace(m.SkillID) != "" {
			name = m.SkillID
		} else if row.CatalogID != "" {
			name = row.CatalogID
		}
		if v := strings.TrimSpace(row.Version); v != "" {
			name += "-" + v
		}
	}
	return name + ".zip"
}

func uniqueTenantIDs(in []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(in))
	out := make([]uint64, 0, len(in))
	for _, id := range in {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
