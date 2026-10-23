package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/application/service"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
)

func newCatalogArchiveRouter(catalog *fakeSkillCatalog) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())
	r.Use(func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), testSkillTenantID)
		c.Next()
	})
	h := NewSkillHandler(&fakeUsableSkillLister{}, catalog, nil)
	r.GET("/skills/catalog/:id/archive", h.GetCatalogArchive)
	return r
}

func sha256Of(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// VAL-208/INT-103 happy path: the download carries the exact stored bytes,
// the real digest in X-SHA256, the honest Content-Length and a ZIP content
// type — and the service receives the normalized expected digest.
func TestGetCatalogArchiveServesOriginalBytes(t *testing.T) {
	body := []byte("PK\x03\x04real-zip-bytes")
	catalog := &fakeSkillCatalog{archive: &service.CatalogArchive{
		Archive: body, SHA256: sha256Of(body), FileName: "c1-a1b2c3d4e5f6.zip",
	}}
	router := newCatalogArchiveRouter(catalog)

	req := httptest.NewRequest(http.MethodGet,
		"/skills/catalog/cat-1/archive?expected_sha256="+sha256Of(body), nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/zip", w.Header().Get("Content-Type"))
	require.Equal(t, sha256Of(body), w.Header().Get("X-SHA256"))
	require.Equal(t, strconv.Itoa(len(body)), w.Header().Get("Content-Length"))
	require.Equal(t, body, w.Body.Bytes())
	require.Equal(t, "cat-1", catalog.gotCatalogID)
	require.Equal(t, sha256Of(body), catalog.gotExpected)
}

// VAL-202/204: an uppercase precondition is normalized to lowercase; a
// missing or malformed one is INVALID_REQUEST and never reaches the service.
func TestGetCatalogArchiveValidatesExpectedSHA(t *testing.T) {
	catalog := &fakeSkillCatalog{archive: &service.CatalogArchive{
		Archive: []byte("x"), SHA256: sha256Of([]byte("x")), FileName: "x.zip",
	}}
	router := newCatalogArchiveRouter(catalog)

	upper := httptest.NewRequest(http.MethodGet,
		"/skills/catalog/cat-1/archive?expected_sha256="+upperASCII(sha256Of([]byte("x"))), nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, upper)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, sha256Of([]byte("x")), catalog.gotExpected)

	for _, query := range []string{
		"",
		"expected_sha256=",
		"expected_sha256=not-hex",
		"expected_sha256=abcd", // too short
	} {
		catalog.archiveCalls = 0
		req := httptest.NewRequest(http.MethodGet, "/skills/catalog/cat-1/archive?"+query, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusBadRequest, w.Code, "query %q", query)
		require.Equal(t, 0, catalog.archiveCalls, "query %q must not reach the service", query)
		var payload struct {
			Success bool `json:"success"`
			Error   struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
		require.Equal(t, catalogArchiveCodeInvalidRequest, payload.Error.Code)
	}
}

// VAL-204: cross-workspace or missing catalogs share one shape — 404
// RESOURCE_NOT_ACCESSIBLE — so enumeration learns nothing.
func TestGetCatalogArchiveNotFoundIsResourceNotAccessible(t *testing.T) {
	catalog := &fakeSkillCatalog{archiveErr: apperrors.NewNotFoundError("skill catalog is not accessible")}
	router := newCatalogArchiveRouter(catalog)

	req := httptest.NewRequest(http.MethodGet,
		"/skills/catalog/cat-9/archive?expected_sha256="+sha256Of([]byte("x")), nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	require.Equal(t, catalogArchiveCodeNotAccessible, payload.Error.Code)
}

// VAL-210: a stale expected digest maps to 409 SKILL_VERSION_CHANGED.
func TestGetCatalogArchiveVersionChanged(t *testing.T) {
	catalog := &fakeSkillCatalog{archiveErr: service.ErrCatalogVersionChanged}
	router := newCatalogArchiveRouter(catalog)

	req := httptest.NewRequest(http.MethodGet,
		"/skills/catalog/cat-1/archive?expected_sha256="+sha256Of([]byte("old")), nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code)
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	require.Equal(t, catalogArchiveCodeVersionChanged, payload.Error.Code)
}

// VAL-211 family: unreadable archive and digest mismatch are errors, never a
// ZIP-flavoured success.
func TestGetCatalogArchiveServiceFailuresAreNotZips(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not ready", service.ErrCatalogArchiveNotReady, http.StatusConflict, catalogArchiveCodeNotReady},
		{"hash mismatch", service.ErrCatalogArchiveHashMismatch, http.StatusUnprocessableEntity, catalogArchiveCodeHashMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			catalog := &fakeSkillCatalog{archiveErr: tc.err}
			router := newCatalogArchiveRouter(catalog)

			req := httptest.NewRequest(http.MethodGet,
				"/skills/catalog/cat-1/archive?expected_sha256="+sha256Of([]byte("x")), nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			require.Equal(t, tc.status, w.Code)
			require.NotEqual(t, "application/zip", w.Header().Get("Content-Type"))
			var payload struct {
				Success bool `json:"success"`
				Error   struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
			require.False(t, payload.Success)
			require.Equal(t, tc.code, payload.Error.Code)
		})
	}
}

// An unexpected internal failure falls through to the shared error
// middleware; it still must not be wrapped into a 200 ZIP.
func TestGetCatalogArchiveInternalErrorStaysJSON(t *testing.T) {
	catalog := &fakeSkillCatalog{archiveErr: errors.New("storage backend exploded")}
	router := newCatalogArchiveRouter(catalog)

	req := httptest.NewRequest(http.MethodGet,
		"/skills/catalog/cat-1/archive?expected_sha256="+sha256Of([]byte("x")), nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.NotEqual(t, "application/zip", w.Header().Get("Content-Type"))
}

// INT-105 envelope shape: request_id travels INSIDE the error object, as a
// sibling of code and message — never as a top-level field next to "error".
func TestGetCatalogArchiveErrorNestsRequestID(t *testing.T) {
	catalog := &fakeSkillCatalog{archiveErr: service.ErrCatalogArchiveNotReady}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())
	r.Use(func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), testSkillTenantID)
		c.Set(types.RequestIDContextKey.String(), "req-123")
		c.Next()
	})
	h := NewSkillHandler(&fakeUsableSkillLister{}, catalog, nil)
	r.GET("/skills/catalog/:id/archive", h.GetCatalogArchive)

	req := httptest.NewRequest(http.MethodGet,
		"/skills/catalog/cat-1/archive?expected_sha256="+sha256Of([]byte("x")), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code)
	var payload struct {
		Success   bool `json:"success"`
		RequestID any  `json:"request_id"`
		Error     struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	require.False(t, payload.Success)
	require.Equal(t, catalogArchiveCodeNotReady, payload.Error.Code)
	require.Equal(t, "req-123", payload.Error.RequestID)
	require.Nil(t, payload.RequestID, "request_id must not also appear at the top level")
}

// Hostile ids are rejected before the service is consulted.
func TestGetCatalogArchiveRejectsHostileIDs(t *testing.T) {
	catalog := &fakeSkillCatalog{archive: &service.CatalogArchive{
		Archive: []byte("x"), SHA256: sha256Of([]byte("x")), FileName: "x.zip",
	}}
	router := newCatalogArchiveRouter(catalog)

	for _, id := range []string{
		"..%2F..%2Fetc",
		"a%00b",
	} {
		req := httptest.NewRequest(http.MethodGet,
			"/skills/catalog/"+id+"/archive?expected_sha256="+sha256Of([]byte("x")), nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusOK, w.Code, "id %q", id)
	}
	require.Zero(t, catalog.archiveCalls)
}

func upperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - ('a' - 'A')
		}
	}
	return string(b)
}
