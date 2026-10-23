package handler

import (
	stderrors "errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/application/service"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
)

// Enumerated string codes of the catalog archive distribution endpoint
// (WORK-RESOURCES-001 INT-105). They travel in the standard error envelope's
// code field.
const (
	catalogArchiveCodeInvalidRequest = "INVALID_REQUEST"
	catalogArchiveCodeNotAccessible  = "RESOURCE_NOT_ACCESSIBLE"
	catalogArchiveCodeNotReady       = "ARCHIVE_NOT_READY"
	catalogArchiveCodeVersionChanged = "SKILL_VERSION_CHANGED"
	catalogArchiveCodeHashMismatch   = "SKILL_HASH_MISMATCH"

	catalogArchiveMaxCatalogIDLen = 128
	catalogArchiveContentType     = "application/zip"
)

// catalogArchiveErrorBody builds the INT-105 error envelope for the archive
// route: a machine-readable code, a message free of storage internals, and
// the request id when one is on the context. Per INT-105 the request id
// travels INSIDE the error object, alongside code and message.
func catalogArchiveErrorBody(c *gin.Context, code, message string) gin.H {
	errObj := gin.H{
		"code":    code,
		"message": message,
	}
	if id, ok := c.Get(types.RequestIDContextKey.String()); ok {
		if s, ok := id.(string); ok && s != "" {
			errObj["request_id"] = s
		}
	}
	return gin.H{
		"success": false,
		"error":   errObj,
	}
}

// validCatalogArchiveID reports whether raw is safe to use as a catalog id:
// a single non-empty segment, no path separators, no dot-dot, no control or
// whitespace characters, bounded in length (DATA-101).
func validCatalogArchiveID(raw string) bool {
	if raw == "" || len(raw) > catalogArchiveMaxCatalogIDLen || strings.TrimSpace(raw) != raw {
		return false
	}
	if strings.Contains(raw, "..") || strings.ContainsAny(raw, `/\`) {
		return false
	}
	for _, r := range raw {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

// normalizeExpectedSHA256 lowercases and validates a caller-supplied digest
// precondition. The second return is false when the value is malformed.
func normalizeExpectedSHA256(raw string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if len(v) != 64 {
		return "", false
	}
	for _, r := range v {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", false
		}
	}
	return v, true
}

// respondCatalogArchiveError maps service failures onto INT-105 codes. It
// reports whether err was one of the mapped archive failures; anything else
// falls through to the shared error middleware.
func respondCatalogArchiveError(c *gin.Context, err error) bool {
	switch {
	case stderrors.Is(err, service.ErrCatalogArchiveNotReady):
		c.JSON(http.StatusConflict, catalogArchiveErrorBody(c,
			catalogArchiveCodeNotReady, "the archive of this skill catalog is not ready yet"))
	case stderrors.Is(err, service.ErrCatalogVersionChanged):
		c.JSON(http.StatusConflict, catalogArchiveErrorBody(c,
			catalogArchiveCodeVersionChanged, "the skill catalog has a different version now; refresh and confirm again"))
	case stderrors.Is(err, service.ErrCatalogArchiveHashMismatch):
		c.JSON(http.StatusUnprocessableEntity, catalogArchiveErrorBody(c,
			catalogArchiveCodeHashMismatch, "the stored archive does not match the catalog digest"))
	case isNotAccessibleError(err):
		c.JSON(http.StatusNotFound, catalogArchiveErrorBody(c,
			catalogArchiveCodeNotAccessible, "skill catalog is not accessible"))
	default:
		return false
	}
	return true
}

// isNotAccessibleError matches the shared not-found shape the service uses
// for a missing or cross-workspace catalog, which this route surfaces as 404
// RESOURCE_NOT_ACCESSIBLE so enumeration learns nothing.
func isNotAccessibleError(err error) bool {
	appErr, ok := apperrors.IsAppError(err)
	return ok && appErr.HTTPCode == http.StatusNotFound
}

// GetCatalogArchive godoc
// @Summary      Download the original archive of a workspace skill catalog
// @Description  Returns the authoritative stored bundle bytes of one live catalog definition, strictly resolved by id in the caller's workspace. The caller's expected_sha256 must match the current catalog digest. The response carries the digest in X-SHA256 and the byte count in Content-Length. Missing or unreadable archives answer 409 ARCHIVE_NOT_READY; a stale precondition answers 409 SKILL_VERSION_CHANGED; bytes that no longer hash to the recorded digest answer 422 SKILL_HASH_MISMATCH; a missing or invisible catalog answers 404 RESOURCE_NOT_ACCESSIBLE.
// @Tags         Skills
// @Produce      application/zip
// @Param        id                path   string  true  "Catalog skill ID"
// @Param        expected_sha256   query  string  true  "Expected lowercase hex SHA-256 from the listing the user confirmed"
// @Success      200  {file}  binary
// @Failure      400  {object}  map[string]interface{}  "INVALID_REQUEST"
// @Failure      404  {object}  map[string]interface{}  "RESOURCE_NOT_ACCESSIBLE"
// @Failure      409  {object}  map[string]interface{}  "ARCHIVE_NOT_READY / SKILL_VERSION_CHANGED"
// @Failure      422  {object}  map[string]interface{}  "SKILL_HASH_MISMATCH"
// @Security     Bearer
// @Router       /skills/catalog/{id}/archive [get]
func (h *SkillHandler) GetCatalogArchive(c *gin.Context) {
	if h.catalog == nil {
		_ = c.Error(apperrors.NewInternalServerError("skill catalog is not configured"))
		return
	}
	id := c.Param("id")
	if !validCatalogArchiveID(id) {
		c.JSON(http.StatusBadRequest, catalogArchiveErrorBody(c,
			catalogArchiveCodeInvalidRequest, "catalog id is invalid"))
		return
	}
	expected, ok := normalizeExpectedSHA256(c.Query("expected_sha256"))
	if !ok {
		c.JSON(http.StatusBadRequest, catalogArchiveErrorBody(c,
			catalogArchiveCodeInvalidRequest, "expected_sha256 must be a 64-character lowercase hex SHA-256"))
		return
	}

	archive, err := h.catalog.OpenCatalogArchive(
		c.Request.Context(), sandboxConfigTenantID(c), id, expected)
	if err != nil {
		if respondCatalogArchiveError(c, err) {
			return
		}
		_ = c.Error(err)
		return
	}
	if archive == nil || len(archive.Archive) == 0 {
		c.JSON(http.StatusConflict, catalogArchiveErrorBody(c,
			catalogArchiveCodeNotReady, "the archive of this skill catalog is not ready yet"))
		return
	}
	// The bytes are already fully verified before this point, so the
	// response headers can state their digest and length honestly. A service
	// failure above can never be wrapped into a ZIP-looking 200.
	c.Header("Content-Type", catalogArchiveContentType)
	c.Header("X-SHA256", archive.SHA256)
	c.Header("Content-Disposition", "attachment; filename=\""+archive.FileName+"\"")
	c.Header("Content-Length", strconv.Itoa(len(archive.Archive)))
	c.Status(http.StatusOK)
	_, _ = c.Writer.Write(archive.Archive)
}
