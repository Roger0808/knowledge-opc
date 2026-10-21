package service

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/application/repository"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// releaseFileService is an in-memory FileService; objects stands in for the
// tenant-skills/catalog object store.
type releaseFileService struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newReleaseFileService() *releaseFileService {
	return &releaseFileService{objects: map[string][]byte{}}
}

func (s *releaseFileService) CheckConnectivity(context.Context) error { return nil }

func (s *releaseFileService) SaveFile(context.Context, *multipart.FileHeader, uint64, string) (string, error) {
	return "", nil
}

func (s *releaseFileService) SaveBytes(_ context.Context, data []byte, _ uint64, name string, _ bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := "file://" + name
	copied := make([]byte, len(data))
	copy(copied, data)
	s.objects[ref] = copied
	return ref, nil
}

func (s *releaseFileService) GetFile(_ context.Context, ref string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[ref]
	if !ok {
		return nil, apperrors.NewNotFoundError("stored object not found")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *releaseFileService) GetFileURL(context.Context, string) (string, error) { return "", nil }

func (s *releaseFileService) DeleteFile(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, ref)
	return nil
}

func (s *releaseFileService) CopyFile(_ context.Context, srcPath string, _ uint64, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[srcPath]
	if !ok {
		return "", apperrors.NewNotFoundError("stored object not found")
	}
	ref := "file://copy-" + name
	copied := make([]byte, len(data))
	copy(copied, data)
	s.objects[ref] = copied
	return ref, nil
}

func (s *releaseFileService) put(ref string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make([]byte, len(data))
	copy(copied, data)
	s.objects[ref] = copied
}

type releaseStorageResolver struct {
	fs   interfaces.FileService
	mu   sync.Mutex
	seen []uint64
}

func (r *releaseStorageResolver) ResolveFileService(
	_ context.Context, tenant *types.Tenant, _, _, _ string,
) (interfaces.FileService, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var id uint64
	if tenant != nil {
		id = tenant.ID
	}
	r.seen = append(r.seen, id)
	return r.fs, "", nil
}

func (r *releaseStorageResolver) ResolveBackend(context.Context, *types.Tenant, string, string) (*types.StorageBackend, error) {
	return nil, nil
}

// lastSeenTenant is the tenant the file service was last resolved for.
func (r *releaseStorageResolver) lastSeenTenant() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		return 0
	}
	return r.seen[len(r.seen)-1]
}

type releaseFixture struct {
	db       *gorm.DB
	repo     repository.SkillReleaseRepository
	svc      *SkillReleaseService
	files    *releaseFileService
	resolver *releaseStorageResolver
	now      time.Time
}

func newReleaseFixture(t *testing.T) *releaseFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:skill-release-"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	// The schema comes from the real SQLite migration chain (000028 creates
	// tenant_skill_catalog, 000046 skill_release), not a GORM AutoMigrate
	// approximation of it.
	applySQLiteMigration(t, db, "000028_tenant_skills_and_env_vars.up.sql")
	applySQLiteMigration(t, db, "000046_opc_integration.up.sql")
	fx := &releaseFixture{
		db:       db,
		repo:     repository.NewSkillReleaseRepository(db),
		files:    newReleaseFileService(),
		resolver: &releaseStorageResolver{},
		now:      time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
	}
	fx.resolver.fs = fx.files
	fx.svc = NewSkillReleaseService(fx.repo, fx.resolver)
	fx.svc.now = func() time.Time { return fx.now }
	return fx
}

const releaseSkillMD = `---
name: pdf-tools
version: 1.0.0
description: Extract text from PDF files
---

Use scripts/extract.py to pull text out of a PDF.
`

// makeReleaseArchive builds a publishable bundle the way the OPC packager
// does: payload zip (everything except opc-client.json) → digest → manifest
// carrying that digest → final zip. The returned digest is what a correct
// opc-client.json must attest.
func makeReleaseArchive(t *testing.T, skillMD string, manifest map[string]any) ([]byte, string) {
	t.Helper()
	payload, err := zipSkillFiles(map[string][]byte{"SKILL.md": []byte(skillMD)})
	require.NoError(t, err)
	digest := skillArchiveSHA256(payload)
	raw, err := json.Marshal(manifestWithDigest(manifest, digest))
	require.NoError(t, err)
	archive, err := zipSkillFiles(map[string][]byte{
		"SKILL.md":        []byte(skillMD),
		"opc-client.json": raw,
	})
	require.NoError(t, err)
	return archive, digest
}

func manifestWithDigest(base map[string]any, digest string) map[string]any {
	out := map[string]any{
		"skill_id":                "pdf-tools",
		"version":                 "1.0.0",
		"sha256":                  digest,
		"exec_mode":               "local_sandbox",
		"clients":                 []string{"work", "code"},
		"allowed_tool_categories": []string{"file"},
		"dependencies":            []string{"pypdf==4.0.0"},
		"min_client_protocol":     "1.0",
	}
	for k, v := range base {
		out[k] = v
	}
	return out
}

func validManifest() map[string]any { return map[string]any{} }

// seedCatalog stores a catalog archive and the catalog row naming it.
func (fx *releaseFixture) seedCatalog(t *testing.T, tenantID uint64, catalogID, version string, archive []byte) *types.TenantSkillCatalogEntity {
	t.Helper()
	ref, err := fx.files.SaveBytes(context.Background(), archive, tenantID,
		"tenant-skills/catalog/"+catalogID+".zip", false)
	require.NoError(t, err)
	row := &types.TenantSkillCatalogEntity{
		ID: catalogID, TenantID: tenantID, Name: "pdf-tools", Version: version,
		Description: "Extract text from PDF files",
		BundleRef:   ref, BundleSHA256: skillArchiveSHA256(archive),
		CreatedAt: fx.now, UpdatedAt: fx.now,
	}
	require.NoError(t, fx.db.Create(row).Error)
	return row
}

func (fx *releaseFixture) publish(t *testing.T, tenantID uint64, key, catalogID string, in SkillReleasePublishInput) (*SkillReleasePublishResult, error) {
	t.Helper()
	in.IdempotencyKey = key
	return fx.svc.Publish(context.Background(), tenantID, "user-1", catalogID, in)
}

func TestSkillReleasePublishHappyPath(t *testing.T) {
	fx := newReleaseFixture(t)
	archive, digest := makeReleaseArchive(t, releaseSkillMD, validManifest())
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", archive)

	result, err := fx.publish(t, 7, "key-1", "cat-1", SkillReleasePublishInput{
		TargetTenants: []uint64{9, 9}, TargetRoles: []string{"admin", "admin"},
	})
	require.NoError(t, err)
	require.True(t, result.Created)
	row := result.Release
	require.NotEmpty(t, row.ID)
	require.Equal(t, uint64(7), row.TenantID)
	require.Equal(t, "cat-1", row.CatalogID)
	require.Equal(t, "1.0.0", row.Version)
	// bundle_sha256 is the server-side digest of the actual archive bytes.
	require.Equal(t, skillArchiveSHA256(archive), row.BundleSHA256)
	require.Equal(t, digest, row.Manifest.SHA256)
	require.Equal(t, "pdf-tools", row.Manifest.SkillID)
	require.Equal(t, "local_sandbox", row.Manifest.ExecMode)
	require.Equal(t, types.SkillReleaseStatusPublished, row.Status)
	require.Equal(t, "user-1", row.PublishedBy)
	require.Equal(t, fx.now, row.PublishedAt)
	require.Equal(t, types.SkillReleaseTenantList{9}, row.TargetTenants)
	require.Equal(t, types.SkillReleaseRoleList{"admin"}, row.TargetRoles)
	// F2: the release materializes the archive to its own immutable key, so
	// later catalog re-uploads or deletions cannot corrupt published bytes.
	require.NotEmpty(t, row.ArchiveRef)
	require.Contains(t, row.ArchiveRef, "tenant-skills/releases/")
}

func TestSkillReleasePublishMissingCatalog(t *testing.T) {
	fx := newReleaseFixture(t)
	_, err := fx.publish(t, 7, "", "nope", SkillReleasePublishInput{})
	var appErr *apperrors.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, apperrors.ErrNotFound, appErr.Code)
}

func TestSkillReleasePublishCatalogWithoutArchive(t *testing.T) {
	fx := newReleaseFixture(t)
	require.NoError(t, fx.db.Create(&types.TenantSkillCatalogEntity{
		ID: "cat-empty", TenantID: 7, Name: "pdf-tools", Version: "1.0.0",
		CreatedAt: fx.now, UpdatedAt: fx.now,
	}).Error)
	_, err := fx.publish(t, 7, "", "cat-empty", SkillReleasePublishInput{})
	var appErr *apperrors.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, apperrors.ErrBadRequest, appErr.Code)
}

func TestSkillReleasePublishIdempotencyKeyReplay(t *testing.T) {
	fx := newReleaseFixture(t)
	archive, _ := makeReleaseArchive(t, releaseSkillMD, validManifest())
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", archive)

	first, err := fx.publish(t, 7, "key-1", "cat-1", SkillReleasePublishInput{})
	require.NoError(t, err)
	require.True(t, first.Created)

	second, err := fx.publish(t, 7, "key-1", "cat-1", SkillReleasePublishInput{})
	require.NoError(t, err)
	require.False(t, second.Created)
	require.Equal(t, first.Release.ID, second.Release.ID)

	rows, err := fx.repo.ListSkillReleases(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestSkillReleasePublishIdempotencyKeyForeignTenantConflicts(t *testing.T) {
	fx := newReleaseFixture(t)
	archive, _ := makeReleaseArchive(t, releaseSkillMD, validManifest())
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", archive)
	// The same skill exists in another workspace under another catalog id.
	fx.seedCatalog(t, 8, "cat-9", "1.0.0", archive)

	_, err := fx.publish(t, 7, "key-1", "cat-1", SkillReleasePublishInput{})
	require.NoError(t, err)

	// Replaying the key from another workspace must not return tenant 7's
	// release: publishing is an Admin action scoped to the source tenant.
	_, err = fx.publish(t, 8, "key-1", "cat-9", SkillReleasePublishInput{})
	appErr, ok := apperrors.IsAppError(err)
	require.True(t, ok)
	require.Equal(t, 409, appErr.HTTPCode)
}

func TestSkillReleaseOpenArchiveResolvesPublisherTenantStorage(t *testing.T) {
	fx := newReleaseFixture(t)
	archive, _ := makeReleaseArchive(t, releaseSkillMD, validManifest())
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", archive)
	published, err := fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{TargetTenants: []uint64{9}})
	require.NoError(t, err)

	// A target tenant downloads: the object lives in the publishing
	// workspace's storage, so the file service must be resolved for the
	// release's source tenant (7), never for the caller (9).
	_, err = fx.svc.OpenArchive(context.Background(), 9, types.TenantRoleViewer, published.Release.ID)
	require.NoError(t, err)
	require.Equal(t, uint64(7), fx.resolver.lastSeenTenant())
}

func TestSkillReleasePublishSameVersionSameBytesIsIdempotent(t *testing.T) {
	fx := newReleaseFixture(t)
	archive, _ := makeReleaseArchive(t, releaseSkillMD, validManifest())
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", archive)

	first, err := fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{})
	require.NoError(t, err)
	require.True(t, first.Created)

	second, err := fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{})
	require.NoError(t, err)
	require.False(t, second.Created)
	require.Equal(t, first.Release.ID, second.Release.ID)

	rows, err := fx.repo.ListSkillReleases(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestSkillReleasePublishSameVersionDifferentBytesConflicts(t *testing.T) {
	fx := newReleaseFixture(t)
	v1, _ := makeReleaseArchive(t, releaseSkillMD, validManifest())
	cat := fx.seedCatalog(t, 7, "cat-1", "1.0.0", v1)
	_, err := fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{})
	require.NoError(t, err)

	// Re-registering the catalog replaces the stored bytes under the same ref.
	v2, _ := makeReleaseArchive(t, releaseSkillMD+"\n# v2\n", validManifest())
	require.NotEqual(t, skillArchiveSHA256(v1), skillArchiveSHA256(v2))
	fx.files.put(cat.BundleRef, v2)

	_, err = fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{})
	require.ErrorIs(t, err, ErrSkillReleaseVersionConflict)
}

func TestSkillReleasePublishRejectsBadManifest(t *testing.T) {
	cases := map[string]struct {
		manifest map[string]any
		contains string
	}{
		"missing skill_id":    {manifestWithDigest(map[string]any{"skill_id": ""}, "x"), `required field "skill_id"`},
		"missing version":     {manifestWithDigest(map[string]any{"version": ""}, "x"), `required field "version"`},
		"missing sha256":      {manifestWithDigest(map[string]any{"sha256": ""}, "x"), `required field "sha256"`},
		"missing exec_mode":   {manifestWithDigest(map[string]any{"exec_mode": ""}, "x"), `required field "exec_mode"`},
		"bad exec_mode":       {manifestWithDigest(map[string]any{"exec_mode": "cloud_maybe"}, "x"), "is not an allowed exec_mode"},
		"bad client":          {manifestWithDigest(map[string]any{"clients": []string{"web"}}, "x"), "is not one of"},
		"digest not attested": {manifestWithDigest(validManifest(), strings.Repeat("0", 64)), "does not match the bundle payload digest"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newReleaseFixture(t)
			raw, err := json.Marshal(tc.manifest)
			require.NoError(t, err)
			archive, err := zipSkillFiles(map[string][]byte{
				"SKILL.md":        []byte(releaseSkillMD),
				"opc-client.json": raw,
			})
			require.NoError(t, err)
			fx.seedCatalog(t, 7, "cat-1", "1.0.0", archive)

			_, err = fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{})
			require.ErrorIs(t, err, ErrSkillReleaseManifestInvalid)
			require.ErrorContains(t, err, tc.contains)

			rows, listErr := fx.repo.ListSkillReleases(context.Background())
			require.NoError(t, listErr)
			require.Empty(t, rows)
		})
	}
}

func TestSkillReleasePublishMissingManifestFile(t *testing.T) {
	fx := newReleaseFixture(t)
	archive, err := zipSkillFiles(map[string][]byte{"SKILL.md": []byte(releaseSkillMD)})
	require.NoError(t, err)
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", archive)

	_, err = fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{})
	require.ErrorIs(t, err, ErrSkillReleaseManifestInvalid)
	require.ErrorContains(t, err, "no opc-client.json")
}

func TestSkillReleasePublishRejectsEscapingZip(t *testing.T) {
	fx := newReleaseFixture(t)
	// A bundle whose zip escapes the archive root must fail the shared
	// ParseSkillBundle gate and never reach the manifest check or the store.
	archive := zipReleaseWithEscapeEntry(t)
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", archive)

	_, err := fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{})
	require.ErrorIs(t, err, ErrSkillBundleInvalid)

	rows, listErr := fx.repo.ListSkillReleases(context.Background())
	require.NoError(t, listErr)
	require.Empty(t, rows)
}

func zipReleaseWithEscapeEntry(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	_, err := w.Create("SKILL.md")
	require.NoError(t, err)
	_, err = w.Create("../escape.txt")
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func TestSkillReleaseListVisibility(t *testing.T) {
	fx := newReleaseFixture(t)
	own, _ := makeReleaseArchive(t, releaseSkillMD, validManifest())
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", own)
	_, err := fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{})
	require.NoError(t, err)

	shared, _ := makeReleaseArchive(t, releaseSkillMD+"\n# shared\n", validManifest())
	fx.seedCatalog(t, 8, "cat-2", "1.0.0", shared)
	_, err = fx.publish(t, 8, "", "cat-2", SkillReleasePublishInput{TargetTenants: []uint64{9}})
	require.NoError(t, err)

	forTenant := func(id uint64) []string {
		rows, err := fx.svc.ListReleases(context.Background(), id)
		require.NoError(t, err)
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, row.CatalogID)
		}
		return out
	}
	require.Equal(t, []string{"cat-1"}, forTenant(7))
	require.Equal(t, []string{"cat-2"}, forTenant(9))
	require.Empty(t, forTenant(10))
}

func TestSkillReleaseGetEnforcesVisibility(t *testing.T) {
	fx := newReleaseFixture(t)
	own, _ := makeReleaseArchive(t, releaseSkillMD, validManifest())
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", own)
	published, err := fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{})
	require.NoError(t, err)

	row, err := fx.svc.GetRelease(context.Background(), 7, published.Release.ID)
	require.NoError(t, err)
	require.Equal(t, published.Release.ID, row.ID)

	_, err = fx.svc.GetRelease(context.Background(), 9, published.Release.ID)
	var appErr *apperrors.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, apperrors.ErrNotFound, appErr.Code)
}

func TestSkillReleaseOpenArchiveStreamsBytes(t *testing.T) {
	fx := newReleaseFixture(t)
	archive, _ := makeReleaseArchive(t, releaseSkillMD, validManifest())
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", archive)
	published, err := fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{})
	require.NoError(t, err)

	opened, err := fx.svc.OpenArchive(context.Background(), 7, types.TenantRoleViewer, published.Release.ID)
	require.NoError(t, err)
	require.Equal(t, int64(len(archive)), opened.Size)
	require.Equal(t, skillArchiveSHA256(archive), opened.SHA256)
	body, err := io.ReadAll(opened.Body)
	require.NoError(t, err)
	require.Equal(t, archive, body)
}

func TestParseSkillReleaseManifest(t *testing.T) {
	valid := func() map[string]any {
		return map[string]any{
			"skill_id":            "pdf-tools",
			"version":             "1.0.0",
			"sha256":              strings.Repeat("a", 64),
			"exec_mode":           "local_sandbox",
			"clients":             []string{"work"},
			"min_client_protocol": "1.0",
		}
	}
	raw := func(m map[string]any) []byte {
		b, err := json.Marshal(m)
		require.NoError(t, err)
		return b
	}

	t.Run("accepts a valid manifest", func(t *testing.T) {
		m, err := ParseSkillReleaseManifest(raw(valid()))
		require.NoError(t, err)
		require.Equal(t, "pdf-tools", m.SkillID)
		require.Equal(t, "1.0.0", m.Version)
		require.Equal(t, "local_sandbox", m.ExecMode)
		require.Equal(t, []string{"work"}, m.Clients)
		require.Equal(t, "1.0", m.MinClientProtocol)
	})

	t.Run("accepts every documented exec mode", func(t *testing.T) {
		for _, mode := range SkillReleaseExecModes {
			m := valid()
			m["exec_mode"] = mode
			_, err := ParseSkillReleaseManifest(raw(m))
			require.NoError(t, err, mode)
		}
	})

	for name, mutate := range map[string]func(map[string]any){
		"rejects missing skill_id":  func(m map[string]any) { m["skill_id"] = "" },
		"rejects missing version":   func(m map[string]any) { delete(m, "version") },
		"rejects missing sha256":    func(m map[string]any) { m["sha256"] = " " },
		"rejects missing exec_mode": func(m map[string]any) { delete(m, "exec_mode") },
		"rejects unknown exec_mode": func(m map[string]any) { m["exec_mode"] = "edge_runtime" },
		"rejects unknown client":    func(m map[string]any) { m["clients"] = []string{"work", "phone"} },
		"rejects malformed json":    func(m map[string]any) {},
	} {
		t.Run(name, func(t *testing.T) {
			var data []byte
			if name == "rejects malformed json" {
				data = []byte("{not json")
			} else {
				m := valid()
				mutate(m)
				data = raw(m)
			}
			_, err := ParseSkillReleaseManifest(data)
			require.ErrorIs(t, err, ErrSkillReleaseManifestInvalid)
		})
	}
}

func TestSkillReleasePublishRejectsSymlinkZip(t *testing.T) {
	fx := newReleaseFixture(t)
	// zipBundleWithSymlink builds a bundle whose scripts/link entry is a
	// symlink; the shared ParseSkillBundle gate must refuse it at publish
	// time, before any manifest check or store write.
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", zipBundleWithSymlink(t))

	_, err := fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{})
	require.ErrorIs(t, err, ErrSkillBundleInvalid)

	rows, listErr := fx.repo.ListSkillReleases(context.Background())
	require.NoError(t, listErr)
	require.Empty(t, rows)
}

func TestSkillReleaseOpenArchiveDelistedForbidden(t *testing.T) {
	fx := newReleaseFixture(t)
	archive, _ := makeReleaseArchive(t, releaseSkillMD, validManifest())
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", archive)
	published, err := fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{TargetTenants: []uint64{9}})
	require.NoError(t, err)

	_, err = fx.svc.Delist(context.Background(), 7, published.Release.ID)
	require.NoError(t, err)

	_, err = fx.svc.OpenArchive(context.Background(), 7, types.TenantRoleViewer, published.Release.ID)
	require.ErrorIs(t, err, ErrSkillReleaseDelisted)

	// Target tenants hit the same wall.
	_, err = fx.svc.OpenArchive(context.Background(), 9, types.TenantRoleViewer, published.Release.ID)
	require.ErrorIs(t, err, ErrSkillReleaseDelisted)
}

func TestSkillReleaseOpenArchiveHashMismatch(t *testing.T) {
	fx := newReleaseFixture(t)
	archive, _ := makeReleaseArchive(t, releaseSkillMD, validManifest())
	cat := fx.seedCatalog(t, 7, "cat-1", "1.0.0", archive)
	published, err := fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{})
	require.NoError(t, err)

	// F2: the catalog object is no longer the release's source of truth —
	// replacing it must not corrupt the published bytes.
	tampered, _ := makeReleaseArchive(t, releaseSkillMD+"\n# tampered\n", validManifest())
	fx.files.put(cat.BundleRef, tampered)
	if _, err := fx.svc.OpenArchive(context.Background(), 7, types.TenantRoleViewer, published.Release.ID); err != nil {
		t.Fatalf("catalog tamper must not affect the materialized release: %v", err)
	}

	// Tampering with the release's own immutable key is still caught.
	fx.files.put(published.Release.ArchiveRef, tampered)
	_, err = fx.svc.OpenArchive(context.Background(), 7, types.TenantRoleViewer, published.Release.ID)
	require.ErrorIs(t, err, ErrSkillReleaseHashMismatch)
}

func TestSkillReleaseOpenArchiveRoleRestriction(t *testing.T) {
	fx := newReleaseFixture(t)
	archive, _ := makeReleaseArchive(t, releaseSkillMD, validManifest())
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", archive)
	published, err := fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{TargetRoles: []string{"admin"}})
	require.NoError(t, err)

	_, err = fx.svc.OpenArchive(context.Background(), 7, types.TenantRoleViewer, published.Release.ID)
	var appErr *apperrors.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, apperrors.ErrForbidden, appErr.Code)

	_, err = fx.svc.OpenArchive(context.Background(), 7, types.TenantRoleAdmin, published.Release.ID)
	require.NoError(t, err)
}

func TestSkillReleaseDelistOnlySourceTenant(t *testing.T) {
	fx := newReleaseFixture(t)
	archive, _ := makeReleaseArchive(t, releaseSkillMD, validManifest())
	fx.seedCatalog(t, 7, "cat-1", "1.0.0", archive)
	published, err := fx.publish(t, 7, "", "cat-1", SkillReleasePublishInput{TargetTenants: []uint64{9}})
	require.NoError(t, err)

	_, err = fx.svc.Delist(context.Background(), 9, published.Release.ID)
	var appErr *apperrors.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, apperrors.ErrNotFound, appErr.Code)

	row, err := fx.svc.Delist(context.Background(), 7, published.Release.ID)
	require.NoError(t, err)
	require.Equal(t, types.SkillReleaseStatusDelisted, row.Status)
	require.NotNil(t, row.DelistedAt)
	require.Equal(t, fx.now, *row.DelistedAt)

	// Delisting is idempotent.
	again, err := fx.svc.Delist(context.Background(), 7, published.Release.ID)
	require.NoError(t, err)
	require.Equal(t, row.DelistedAt, again.DelistedAt)
}
