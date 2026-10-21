package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/application/service"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
)

type fakeSkillReleaseService struct {
	publishResult *service.SkillReleasePublishResult
	publishErr    error
	publishInput  service.SkillReleasePublishInput
	publishKey    string

	listRows []*types.SkillReleaseEntity
	listErr  error

	getRow *types.SkillReleaseEntity
	getErr error

	archive *service.SkillReleaseArchive
	openErr error
	role    types.TenantRole

	delistRow *types.SkillReleaseEntity
	delistErr error
}

func (f *fakeSkillReleaseService) Publish(
	_ context.Context, _ uint64, _ string, _ string, in service.SkillReleasePublishInput,
) (*service.SkillReleasePublishResult, error) {
	f.publishInput = in
	return f.publishResult, f.publishErr
}

func (f *fakeSkillReleaseService) ListReleases(context.Context, uint64) ([]*types.SkillReleaseEntity, error) {
	return f.listRows, f.listErr
}

func (f *fakeSkillReleaseService) GetRelease(context.Context, uint64, string) (*types.SkillReleaseEntity, error) {
	return f.getRow, f.getErr
}

func (f *fakeSkillReleaseService) OpenArchive(
	_ context.Context, _ uint64, role types.TenantRole, _ string,
) (*service.SkillReleaseArchive, error) {
	f.role = role
	return f.archive, f.openErr
}

func (f *fakeSkillReleaseService) Delist(context.Context, uint64, string) (*types.SkillReleaseEntity, error) {
	return f.delistRow, f.delistErr
}

func newSkillReleaseRouter(svc skillReleaseService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())
	r.Use(func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), uint64(7))
		c.Next()
	})
	h := NewSkillReleaseHandler(svc)
	r.POST("/skills/catalog/:id/publish", h.PublishCatalogRelease)
	r.GET("/skills/releases", h.ListSkillReleases)
	r.GET("/skills/releases/:id", h.GetSkillRelease)
	r.GET("/skills/releases/:id/archive", h.GetSkillReleaseArchive)
	r.POST("/skills/releases/:id/delist", h.DelistSkillRelease)
	return r
}

func releaseRow(id string) *types.SkillReleaseEntity {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	return &types.SkillReleaseEntity{
		ID:           id,
		TenantID:     7,
		CatalogID:    "cat-1",
		Version:      "1.0.0",
		BundleSHA256: strings.Repeat("a", 64),
		Manifest: types.SkillReleaseManifest{
			SkillID: "pdf-tools", Version: "1.0.0",
			SHA256: strings.Repeat("a", 64), ExecMode: "local_sandbox",
			Clients: []string{"work"},
		},
		Status:      types.SkillReleaseStatusPublished,
		PublishedBy: "user-1",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func errorEnvelopeCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Success bool `json:"success"`
		Error   struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.False(t, body.Success)
	code, ok := body.Error.Code.(string)
	require.True(t, ok, "expected a string error code, got %v", body.Error.Code)
	return code
}

func TestSkillReleasePublishCreated(t *testing.T) {
	svc := &fakeSkillReleaseService{
		publishResult: &service.SkillReleasePublishResult{Release: releaseRow("rel-1"), Created: true},
	}
	router := newSkillReleaseRouter(svc)

	body, err := json.Marshal(map[string]any{"target_tenants": []uint64{9}, "target_roles": []string{"admin"}})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/skills/catalog/cat-1/publish", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "key-1")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, "key-1", svc.publishInput.IdempotencyKey)
	require.Equal(t, []uint64{9}, svc.publishInput.TargetTenants)
	require.Equal(t, []string{"admin"}, svc.publishInput.TargetRoles)
	var resp struct {
		Success bool                 `json:"success"`
		Data    skillReleaseResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.Success)
	require.Equal(t, "rel-1", resp.Data.ID)
	require.Equal(t, "pdf-tools", resp.Data.Manifest.SkillID)
	require.Equal(t, strings.Repeat("a", 64), resp.Data.BundleSHA256)
}

func TestSkillReleasePublishReplayReturnsOK(t *testing.T) {
	svc := &fakeSkillReleaseService{
		publishResult: &service.SkillReleasePublishResult{Release: releaseRow("rel-1"), Created: false},
	}
	router := newSkillReleaseRouter(svc)

	req := httptest.NewRequest(http.MethodPost, "/skills/catalog/cat-1/publish", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
}

func TestSkillReleasePublishVersionConflictEnvelope(t *testing.T) {
	svc := &fakeSkillReleaseService{
		publishErr: errors.Join(service.ErrSkillReleaseVersionConflict),
	}
	router := newSkillReleaseRouter(svc)

	req := httptest.NewRequest(http.MethodPost, "/skills/catalog/cat-1/publish", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code)
	require.Equal(t, "SKILL_VERSION_CONFLICT", errorEnvelopeCode(t, w))
}

func TestSkillReleasePublishManifestInvalidEnvelope(t *testing.T) {
	svc := &fakeSkillReleaseService{
		publishErr: errors.Join(service.ErrSkillReleaseManifestInvalid),
	}
	router := newSkillReleaseRouter(svc)

	req := httptest.NewRequest(http.MethodPost, "/skills/catalog/cat-1/publish", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	require.Equal(t, "SKILL_MANIFEST_INVALID", errorEnvelopeCode(t, w))
}

func TestSkillReleasePublishInvalidJSONBody(t *testing.T) {
	svc := &fakeSkillReleaseService{}
	router := newSkillReleaseRouter(svc)

	req := httptest.NewRequest(http.MethodPost, "/skills/catalog/cat-1/publish",
		strings.NewReader("{broken"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSkillReleaseListAndGet(t *testing.T) {
	svc := &fakeSkillReleaseService{listRows: []*types.SkillReleaseEntity{releaseRow("rel-1")}}
	router := newSkillReleaseRouter(svc)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/skills/releases", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var list struct {
		Success bool                   `json:"success"`
		Data    []skillReleaseResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	require.True(t, list.Success)
	require.Len(t, list.Data, 1)
	require.Equal(t, "rel-1", list.Data[0].ID)

	svc.getRow = releaseRow("rel-2")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/skills/releases/rel-2", nil))
	require.Equal(t, http.StatusOK, w.Code)
}

func TestSkillReleaseGetNotFoundPassesThrough(t *testing.T) {
	svc := &fakeSkillReleaseService{getErr: apperrors.NewNotFoundError("skill release not found")}
	router := newSkillReleaseRouter(svc)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/skills/releases/nope", nil))
	require.Equal(t, http.StatusNotFound, w.Code)
	var body struct {
		Success bool `json:"success"`
		Error   struct {
			Code float64 `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, float64(apperrors.ErrNotFound), body.Error.Code)
}

func TestSkillReleaseListNilService(t *testing.T) {
	router := newSkillReleaseRouter(nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/skills/releases", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"data":[]`)
}

func TestSkillReleaseArchiveDownloadHeaders(t *testing.T) {
	payload := []byte("zip-bytes")
	svc := &fakeSkillReleaseService{
		archive: &service.SkillReleaseArchive{
			Body:     io.NopCloser(strings.NewReader(string(payload))),
			Size:     int64(len(payload)),
			SHA256:   strings.Repeat("b", 64),
			FileName: "pdf-tools-1.0.0.zip",
		},
	}
	router := newSkillReleaseRouter(svc)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/skills/releases/rel-1/archive", nil))

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, strings.Repeat("b", 64), w.Header().Get("X-SHA256"))
	require.Equal(t, "9", w.Header().Get("Content-Length"))
	require.Equal(t, payload, w.Body.Bytes())
}

func TestSkillReleaseArchiveDelistedEnvelope(t *testing.T) {
	svc := &fakeSkillReleaseService{openErr: errors.Join(service.ErrSkillReleaseDelisted)}
	router := newSkillReleaseRouter(svc)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/skills/releases/rel-1/archive", nil))

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, "SKILL_DELISTED", errorEnvelopeCode(t, w))
}

func TestSkillReleaseArchiveHashMismatchEnvelope(t *testing.T) {
	svc := &fakeSkillReleaseService{openErr: errors.Join(service.ErrSkillReleaseHashMismatch)}
	router := newSkillReleaseRouter(svc)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/skills/releases/rel-1/archive", nil))

	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	require.Equal(t, "SKILL_HASH_MISMATCH", errorEnvelopeCode(t, w))
}

func TestSkillReleaseDelist(t *testing.T) {
	row := releaseRow("rel-1")
	row.Status = types.SkillReleaseStatusDelisted
	svc := &fakeSkillReleaseService{delistRow: row}
	router := newSkillReleaseRouter(svc)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/skills/releases/rel-1/delist", nil))

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Success bool                 `json:"success"`
		Data    skillReleaseResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, types.SkillReleaseStatusDelisted, resp.Data.Status)
}
