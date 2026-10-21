package handler

import (
	"context"
	stderrors "errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/application/service"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
)

// Enumerated string codes for skill release failures (SPEC-INT-140). They
// travel in the standard error envelope's code field, alongside the numeric
// AppError codes the rest of the API uses.
const (
	skillReleaseCodeManifestInvalid = "SKILL_MANIFEST_INVALID"
	skillReleaseCodeVersionConflict = "SKILL_VERSION_CONFLICT"
	skillReleaseCodeDelisted        = "SKILL_DELISTED"
	skillReleaseCodeHashMismatch    = "SKILL_HASH_MISMATCH"
	skillReleaseManifestContentType = "application/zip"
	skillReleaseMaxPublishBodyBytes = 1 << 20
)

// skillReleaseService is the release surface the HTTP layer needs. It is
// satisfied by *service.SkillReleaseService.
type skillReleaseService interface {
	Publish(
		ctx context.Context, tenantID uint64, userID, catalogID string, in service.SkillReleasePublishInput,
	) (*service.SkillReleasePublishResult, error)
	ListReleases(ctx context.Context, tenantID uint64) ([]*types.SkillReleaseEntity, error)
	GetRelease(ctx context.Context, tenantID uint64, releaseID string) (*types.SkillReleaseEntity, error)
	OpenArchive(
		ctx context.Context, tenantID uint64, role types.TenantRole, releaseID string,
	) (*service.SkillReleaseArchive, error)
	Delist(ctx context.Context, tenantID uint64, releaseID string) (*types.SkillReleaseEntity, error)
}

// SkillReleaseHandler serves skill release records (EXT-001).
type SkillReleaseHandler struct {
	releases skillReleaseService
}

// NewSkillReleaseHandler wires the handler. releases may be nil in tests that
// only exercise routing.
func NewSkillReleaseHandler(releases skillReleaseService) *SkillReleaseHandler {
	return &SkillReleaseHandler{releases: releases}
}

// skillReleaseResponse is the outward projection of one release row. The
// stored archive reference is deliberately absent: clients address the
// archive through this API, never through storage paths.
type skillReleaseResponse struct {
	ID            string                       `json:"id"`
	CatalogID     string                       `json:"catalog_id"`
	Version       string                       `json:"version"`
	BundleSHA256  string                       `json:"bundle_sha256"`
	Manifest      types.SkillReleaseManifest   `json:"manifest"`
	TargetTenants types.SkillReleaseTenantList `json:"target_tenants"`
	TargetRoles   types.SkillReleaseRoleList   `json:"target_roles"`
	Status        string                       `json:"status"`
	PublishedBy   string                       `json:"published_by"`
	PublishedAt   time.Time                    `json:"published_at"`
	DelistedAt    *time.Time                   `json:"delisted_at,omitempty"`
	CreatedAt     time.Time                    `json:"created_at"`
	UpdatedAt     time.Time                    `json:"updated_at"`
}

func skillReleaseView(row *types.SkillReleaseEntity) skillReleaseResponse {
	if row == nil {
		return skillReleaseResponse{}
	}
	return skillReleaseResponse{
		ID:            row.ID,
		CatalogID:     row.CatalogID,
		Version:       row.Version,
		BundleSHA256:  row.BundleSHA256,
		Manifest:      row.Manifest,
		TargetTenants: row.TargetTenants,
		TargetRoles:   row.TargetRoles,
		Status:        row.Status,
		PublishedBy:   row.PublishedBy,
		PublishedAt:   row.PublishedAt,
		DelistedAt:    row.DelistedAt,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
}

func skillReleaseErrorBody(c *gin.Context, code, message string) gin.H {
	body := gin.H{
		"success": false,
		"error": gin.H{
			"code":    code,
			"message": message,
		},
	}
	// request_id lets a client report be traced back to the server log line.
	if id, ok := c.Get(types.RequestIDContextKey.String()); ok {
		if s, ok := id.(string); ok && s != "" {
			body["request_id"] = s
		}
	}
	return body
}

// respondSkillReleaseError maps the service's typed release failures onto
// their enumerated string codes. It reports whether err was one of them;
// ordinary AppErrors fall through to the shared error middleware.
func respondSkillReleaseError(c *gin.Context, err error) bool {
	switch {
	case stderrors.Is(err, service.ErrSkillReleaseManifestInvalid):
		c.JSON(http.StatusUnprocessableEntity,
			skillReleaseErrorBody(c, skillReleaseCodeManifestInvalid, err.Error()))
	case stderrors.Is(err, service.ErrSkillReleaseVersionConflict):
		c.JSON(http.StatusConflict,
			skillReleaseErrorBody(c, skillReleaseCodeVersionConflict, err.Error()))
	case stderrors.Is(err, service.ErrSkillReleaseDelisted):
		c.JSON(http.StatusForbidden,
			skillReleaseErrorBody(c, skillReleaseCodeDelisted, err.Error()))
	case stderrors.Is(err, service.ErrSkillReleaseHashMismatch):
		c.JSON(http.StatusUnprocessableEntity,
			skillReleaseErrorBody(c, skillReleaseCodeHashMismatch, err.Error()))
	default:
		return false
	}
	return true
}

// skillReleasePublishRequest is the optional JSON body of the publish call.
// Both lists are empty by default: the release is then visible to the source
// workspace only, and downloadable by every role there.
type skillReleasePublishRequest struct {
	TargetTenants []uint64 `json:"target_tenants"`
	TargetRoles   []string `json:"target_roles"`
}

// PublishCatalogRelease godoc
// @Summary      Publish one catalog skill as an immutable release
// @Description  Reads the stored archive back, re-hashes it server-side, validates opc-client.json from the bundle, and records the release. Repeating a publish with the same Idempotency-Key header returns the original release.
// @Tags         Skills
// @Accept       json
// @Produce      json
// @Param        id   path  string  true  "Catalog skill ID"
// @Success      201  {object}  handler.DataResponse{data=handler.skillReleaseResponse}
// @Failure      409  {object}  map[string]interface{}  "SKILL_VERSION_CONFLICT"
// @Failure      422  {object}  map[string]interface{}  "SKILL_MANIFEST_INVALID"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /skills/catalog/{id}/publish [post]
func (h *SkillReleaseHandler) PublishCatalogRelease(c *gin.Context) {
	if h.releases == nil {
		_ = c.Error(apperrors.NewInternalServerError("skill release service is not configured"))
		return
	}
	limitJSONBody(c, skillReleaseMaxPublishBodyBytes)
	var req skillReleasePublishRequest
	if c.Request.ContentLength > 0 || strings.HasPrefix(c.ContentType(), "application/json") {
		if err := c.ShouldBindJSON(&req); err != nil {
			if isRequestBodyTooLarge(err) {
				_ = c.Error(skillJSONRequestTooLargeError())
				return
			}
			_ = c.Error(apperrors.NewBadRequestError("invalid publish request"))
			return
		}
	}
	result, err := h.releases.Publish(c.Request.Context(), sandboxConfigTenantID(c),
		skillReleaseUserID(c), c.Param("id"), service.SkillReleasePublishInput{
			IdempotencyKey: c.GetHeader("Idempotency-Key"),
			TargetTenants:  req.TargetTenants,
			TargetRoles:    req.TargetRoles,
		})
	if err != nil {
		if respondSkillReleaseError(c, err) {
			return
		}
		respondSkillServiceError(c, err)
		return
	}
	if result == nil || result.Release == nil {
		_ = c.Error(apperrors.NewInternalServerError("skill release publish returned no release"))
		return
	}
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	c.JSON(status, gin.H{"success": true, "data": skillReleaseView(result.Release)})
}

// ListSkillReleases godoc
// @Summary      List skill releases visible to this workspace
// @Description  Returns releases this workspace published plus releases targeted at it.
// @Tags         Skills
// @Produce      json
// @Success      200  {object}  handler.DataResponse{data=[]handler.skillReleaseResponse}
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /skills/releases [get]
func (h *SkillReleaseHandler) ListSkillReleases(c *gin.Context) {
	if h.releases == nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": []any{}})
		return
	}
	rows, err := h.releases.ListReleases(c.Request.Context(), sandboxConfigTenantID(c))
	if err != nil {
		_ = c.Error(err)
		return
	}
	out := make([]skillReleaseResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, skillReleaseView(row))
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": out})
}

// GetSkillRelease godoc
// @Summary      Get one skill release
// @Description  Returns the release metadata (manifest, digest, policy) when this workspace is the source or a target tenant.
// @Tags         Skills
// @Produce      json
// @Param        id   path  string  true  "Release ID"
// @Success      200  {object}  handler.DataResponse{data=handler.skillReleaseResponse}
// @Failure      404  {object}  apperrors.AppError  "Skill release not found"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /skills/releases/{id} [get]
func (h *SkillReleaseHandler) GetSkillRelease(c *gin.Context) {
	if h.releases == nil {
		_ = c.Error(apperrors.NewNotFoundError("skill release not found"))
		return
	}
	row, err := h.releases.GetRelease(c.Request.Context(), sandboxConfigTenantID(c), c.Param("id"))
	if err != nil {
		_ = c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": skillReleaseView(row)})
}

// GetSkillReleaseArchive godoc
// @Summary      Download one skill release archive
// @Description  Streams the attested archive bytes. The response carries the digest in X-SHA256 and the byte count in Content-Length. A delisted release answers 403 SKILL_DELISTED; bytes that no longer hash to the recorded digest answer 422 SKILL_HASH_MISMATCH.
// @Tags         Skills
// @Produce      application/zip
// @Param        id   path  string  true  "Release ID"
// @Success      200  {file}  binary
// @Failure      403  {object}  map[string]interface{}  "SKILL_DELISTED"
// @Failure      422  {object}  map[string]interface{}  "SKILL_HASH_MISMATCH"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /skills/releases/{id}/archive [get]
func (h *SkillReleaseHandler) GetSkillReleaseArchive(c *gin.Context) {
	if h.releases == nil {
		_ = c.Error(apperrors.NewNotFoundError("skill release not found"))
		return
	}
	archive, err := h.releases.OpenArchive(
		c.Request.Context(), sandboxConfigTenantID(c),
		types.TenantRoleFromContext(c.Request.Context()), c.Param("id"))
	if err != nil {
		if respondSkillReleaseError(c, err) {
			return
		}
		_ = c.Error(err)
		return
	}
	defer func() { _ = archive.Body.Close() }()
	c.Header("Content-Type", skillReleaseManifestContentType)
	c.Header("X-SHA256", archive.SHA256)
	c.Header("Content-Disposition", "attachment; filename=\""+archive.FileName+"\"")
	c.Header("Content-Length", strconv.FormatInt(archive.Size, 10))
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, archive.Body)
}

// DelistSkillRelease godoc
// @Summary      Delist one skill release
// @Description  Stops new downloads of the release. The row, digest, and stored bytes stay for audit; already-downloaded copies are not affected.
// @Tags         Skills
// @Produce      json
// @Param        id   path  string  true  "Release ID"
// @Success      200  {object}  handler.DataResponse{data=handler.skillReleaseResponse}
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /skills/releases/{id}/delist [post]
func (h *SkillReleaseHandler) DelistSkillRelease(c *gin.Context) {
	if h.releases == nil {
		_ = c.Error(apperrors.NewNotFoundError("skill release not found"))
		return
	}
	row, err := h.releases.Delist(c.Request.Context(), sandboxConfigTenantID(c), c.Param("id"))
	if err != nil {
		_ = c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": skillReleaseView(row)})
}

// skillReleaseUserID is who to stamp as the publisher. API-key callers carry
// no user id; the empty string is honest about that.
func skillReleaseUserID(c *gin.Context) string {
	if id, ok := types.UserIDFromContext(c.Request.Context()); ok {
		return id
	}
	return ""
}
