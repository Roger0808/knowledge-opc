package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// archiveFixture is the WORK-RESOURCES-001 acceptance fixture for the strict
// catalog archive path. It builds distinguishable resource samples whose
// digests are computed from real bundle bytes, and injects the failures the
// specification scenarios need: identity switching (tenant ids), catalog
// moves (re-register), archive replacement, storage loss, and timeouts.
type archiveFixture struct {
	t    *testing.T
	repo repository.TenantSkillRepository
	fs   *archiveFileService
	svc  *TenantSkillService
}

func newArchiveFixture(t *testing.T, dsn string) *archiveFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.TenantSkillEntity{}, &types.TenantSkillCatalogEntity{},
		&types.TenantSkillSnapshotEntity{}, &types.TenantUserEnvVar{},
	))
	fs := &archiveFileService{objects: map[string][]byte{}}
	fx := &archiveFixture{t: t, repo: repository.NewTenantSkillRepository(db), fs: fs}
	fx.svc = NewTenantSkillService(
		fx.repo, nil, &archiveStorageResolver{fs: fs}, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		HostSandboxManager{},
	)
	return fx
}

// buildSkillArchiveBytes packs a standard SKILL.md skill bundle. extra files
// land next to SKILL.md. The digest of the returned bytes is the acceptance
// h1/h2 style hash: always computed from the real bytes, never fabricated.
// The SKILL.md carries real YAML frontmatter, matching what the standard
// parser accepts; no opc-client.json or any other release manifest is added.
func buildSkillArchiveBytes(t *testing.T, skillName, instructions string, extra map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	md := "---\nname: " + skillName + "\ndescription: " + skillName + " skill\n---\n\n" + instructions + "\n"
	require.NoError(t, writeZipEntry(zw, "SKILL.md", []byte(md)))
	for name, body := range extra {
		require.NoError(t, writeZipEntry(zw, name, body))
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func writeZipEntry(zw *zip.Writer, name string, body []byte) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// registerCatalog uploads bytes through the real registration path so the
// catalog row, its BundleRef and its recorded digest are exactly what the
// upload pipeline produces — including the real computed hash.
func (fx *archiveFixture) registerCatalog(t *testing.T, tenantID uint64, archive []byte) *types.TenantSkillCatalogEntity {
	t.Helper()
	cat, err := fx.svc.RegisterCatalogFromArchive(context.Background(), tenantID, archive)
	require.NoError(t, err)
	require.NotNil(t, cat)
	return cat
}

// archiveFileService is an in-memory FileService with injectable failures.
// objects is keyed by storage reference; tests mutate it to simulate an
// archive replacement, and the error/hook fields simulate storage loss,
// timeouts and active-write style failures.
type archiveFileService struct {
	mu      sync.Mutex
	objects map[string][]byte
	// rawReaders serves named references from explicit readers (size
	// boundary tests) instead of the object table.
	rawReaders map[string]io.ReadCloser
	// getErr, when set, makes every GetFile fail (archive missing).
	getErr error
	// beforeGet, when set, runs before each GetFile (e.g. swap the bytes a
	// racing re-register would have written).
	beforeGet func()
	// blockFor, when > 0, delays each GetFile read by this long, honouring
	// cancellation so a deadline surfaces as a timeout, not a hang.
	blockFor time.Duration
}

func (s *archiveFileService) CheckConnectivity(context.Context) error { return nil }
func (s *archiveFileService) SaveFile(context.Context, *multipart.FileHeader, uint64, string) (string, error) {
	return "", nil
}

func (s *archiveFileService) SaveBytes(_ context.Context, data []byte, _ uint64, name string, _ bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := fmt.Sprintf("file://tenant-skills/%s-%d", name, len(s.objects))
	copied := make([]byte, len(data))
	copy(copied, data)
	s.objects[ref] = copied
	return ref, nil
}

func (s *archiveFileService) GetFile(ctx context.Context, ref string) (io.ReadCloser, error) {
	if s.beforeGet != nil {
		s.beforeGet()
	}
	if s.rawReaders != nil {
		s.mu.Lock()
		r, ok := s.rawReaders[ref]
		s.mu.Unlock()
		if ok {
			return r, nil
		}
	}
	s.mu.Lock()
	data, ok := s.objects[ref]
	err := s.getErr
	block := s.blockFor
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("object not found")
	}
	if block > 0 {
		select {
		case <-time.After(block):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *archiveFileService) GetFileURL(context.Context, string) (string, error) { return "", nil }

func (s *archiveFileService) DeleteFile(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, ref)
	return nil
}

func (s *archiveFileService) CopyFile(context.Context, string, uint64, string) (string, error) {
	return "", nil
}

// replaceObject swaps the bytes stored under one reference, simulating an
// object store that no longer holds what the catalog row recorded.
func (s *archiveFileService) replaceObject(ref string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make([]byte, len(data))
	copy(copied, data)
	s.objects[ref] = copied
}

// putRaw stores bytes under an explicit reference, simulating the immutable
// per-copy keys the pinning flow hands installs that keep serving an older
// tree.
func (s *archiveFileService) putRaw(ref string, data []byte) string {
	s.replaceObject(ref, data)
	return ref
}

type archiveStorageResolver struct{ fs *archiveFileService }

func (r *archiveStorageResolver) ResolveFileService(
	context.Context, *types.Tenant, string, string, string,
) (interfaces.FileService, string, error) {
	return r.fs, "", nil
}

func (r *archiveStorageResolver) ResolveBackend(
	context.Context, *types.Tenant, string, string,
) (*types.StorageBackend, error) {
	return nil, nil
}
