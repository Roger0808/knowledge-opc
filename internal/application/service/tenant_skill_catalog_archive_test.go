package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
)

// TestOpenCatalogArchiveReturnsOriginalBytes is the VAL-208 happy path: the
// archive of a visible catalog downloads as the exact stored bytes, with the
// digest computed from those bytes — a plain standard SKILL.md package with
// no release manifest and no opc-client.json downloads verbatim.
func TestOpenCatalogArchiveReturnsOriginalBytes(t *testing.T) {
	fx := newArchiveFixture(t, "file:archive-happy?mode=memory&cache=shared")
	ctx := context.Background()
	// C1's initial package h1: real bytes, standard SKILL.md only.
	h1 := buildSkillArchiveBytes(t, "C1", "Summarize product selling points.", map[string][]byte{
		"scripts/run.py": []byte("print('ok')\n"),
	})
	cat := fx.registerCatalog(t, 1001, h1)
	require.Equal(t, sha256Hex(h1), cat.BundleSHA256)
	require.NotContains(t, string(h1), "opc-client.json")

	archive, err := fx.svc.OpenCatalogArchive(ctx, 1001, cat.ID, sha256Hex(h1))
	require.NoError(t, err)
	require.Equal(t, sha256Hex(h1), archive.SHA256)
	require.True(t, bytesEqual(h1, archive.Archive), "downloaded bytes must be the stored bytes verbatim")
}

// TestOpenCatalogArchiveSeparatesSameNameCatalogsAcrossTenants covers the
// cross-space same-name samples: T1 owns C1, T2 owns the same-named C2, and
// neither identity can reach the other's bytes (VAL-204/208 boundary).
func TestOpenCatalogArchiveSeparatesSameNameCatalogsAcrossTenants(t *testing.T) {
	fx := newArchiveFixture(t, "file:archive-cross-space?mode=memory&cache=shared")
	ctx := context.Background()
	h1 := buildSkillArchiveBytes(t, "C1", "Tenant one copy.", nil)
	h2 := buildSkillArchiveBytes(t, "C1", "Tenant two copy, same display name.", nil)
	c1 := fx.registerCatalog(t, 1001, h1)
	c2 := fx.registerCatalog(t, 1002, h2)
	// Same display name in both workspaces, distinct ids and digests.
	c2.Name = "C1"
	require.NoError(t, fx.repo.UpdateCatalog(ctx, c2))

	one, err := fx.svc.OpenCatalogArchive(ctx, 1001, c1.ID, sha256Hex(h1))
	require.NoError(t, err)
	two, err := fx.svc.OpenCatalogArchive(ctx, 1002, c2.ID, sha256Hex(h2))
	require.NoError(t, err)
	require.NotEqual(t, one.FileName, two.FileName)

	// The same id requested from the other workspace is invisible, and the
	// same-name catalog of the other workspace never substitutes.
	_, err = fx.svc.OpenCatalogArchive(ctx, 1002, c1.ID, sha256Hex(h1))
	requireCatalogNotFound(t, err)
	_, err = fx.svc.OpenCatalogArchive(ctx, 1001, c2.ID, sha256Hex(h2))
	requireCatalogNotFound(t, err)
}

// TestOpenCatalogArchiveRefusesStaleExpectedSHA is VAL-210: after the
// workspace re-uploads h2, a download preconditioned on h1 is a version
// conflict; h2 downloads cleanly.
func TestOpenCatalogArchiveRefusesStaleExpectedSHA(t *testing.T) {
	fx := newArchiveFixture(t, "file:archive-version-race?mode=memory&cache=shared")
	ctx := context.Background()
	h1 := buildSkillArchiveBytes(t, "C1", "Version one.", nil)
	h2 := buildSkillArchiveBytes(t, "C1", "Version two, updated content.", nil)
	cat := fx.registerCatalog(t, 1001, h1)
	h1Sum, h2Sum := sha256Hex(h1), sha256Hex(h2)
	require.NotEqual(t, h1Sum, h2Sum)

	moved, err := fx.svc.RegisterCatalogFromArchive(ctx, 1001, h2)
	require.NoError(t, err)
	require.Equal(t, cat.ID, moved.ID)
	require.Equal(t, h2Sum, moved.BundleSHA256)

	_, err = fx.svc.OpenCatalogArchive(ctx, 1001, cat.ID, h1Sum)
	require.ErrorIs(t, err, ErrCatalogVersionChanged)

	archive, err := fx.svc.OpenCatalogArchive(ctx, 1001, cat.ID, h2Sum)
	require.NoError(t, err)
	require.True(t, bytesEqual(h2, archive.Archive))
}

// TestOpenCatalogArchiveDoesNotResurrectDeletedCatalogs is the trap called
// out in the handoff: after the catalog row is deleted, an old sandbox
// install row that still names the same object must NOT bring the download
// back. The strict lookup never consults the install projection.
func TestOpenCatalogArchiveDoesNotResurrectDeletedCatalogs(t *testing.T) {
	fx := newArchiveFixture(t, "file:archive-deleted?mode=memory&cache=shared")
	ctx := context.Background()
	h1 := buildSkillArchiveBytes(t, "C1", "Soon deleted.", nil)
	cat := fx.registerCatalog(t, 1001, h1)
	// Delete the catalog while nothing references it, then leave a sandbox
	// install row pinned to the deleted definition's object — exactly the
	// stale projection that must not resurrect downloads.
	require.NoError(t, fx.svc.DeleteCatalog(ctx, 1001, cat.ID))
	require.NoError(t, fx.repo.CreateSkill(ctx, &types.TenantSkillEntity{
		ID: "sk-legacy", TenantID: 1001, SandboxConfigID: "cfg-a",
		CatalogID: cat.ID, Name: "C1", BundleRef: cat.BundleRef,
		BundleSHA256: cat.BundleSHA256, Status: types.SkillStatusReady, Enabled: true,
	}))

	_, err := fx.svc.OpenCatalogArchive(ctx, 1001, cat.ID, sha256Hex(h1))
	requireCatalogNotFound(t, err)
	// Even without a precondition the answer stays not-found.
	_, err = fx.svc.OpenCatalogArchive(ctx, 1001, cat.ID, "")
	requireCatalogNotFound(t, err)
}

// TestOpenCatalogArchiveReportsMissingArchive: the catalog row is live but
// the stored object is gone — the archive is not ready, never a fabricated
// zip and never a fallback to some other row's bytes.
func TestOpenCatalogArchiveReportsMissingArchive(t *testing.T) {
	fx := newArchiveFixture(t, "file:archive-missing?mode=memory&cache=shared")
	ctx := context.Background()
	h1 := buildSkillArchiveBytes(t, "C1", "Lost object.", nil)
	cat := fx.registerCatalog(t, 1001, h1)
	fx.fs.getErr = errors.New("object store unavailable")

	_, err := fx.svc.OpenCatalogArchive(ctx, 1001, cat.ID, sha256Hex(h1))
	require.ErrorIs(t, err, ErrCatalogArchiveNotReady)
}

// TestOpenCatalogArchiveDetectsReplacedBytes: the object store serves bytes
// that no longer hash to what the authoritative row records. When the row
// has not moved the answer is a hash mismatch (VAL-211 family).
func TestOpenCatalogArchiveDetectsReplacedBytes(t *testing.T) {
	fx := newArchiveFixture(t, "file:archive-replaced?mode=memory&cache=shared")
	ctx := context.Background()
	h1 := buildSkillArchiveBytes(t, "C1", "Original.", nil)
	cat := fx.registerCatalog(t, 1001, h1)
	fx.fs.replaceObject(cat.BundleRef, []byte("tampered bytes"))

	_, err := fx.svc.OpenCatalogArchive(ctx, 1001, cat.ID, sha256Hex(h1))
	require.ErrorIs(t, err, ErrCatalogArchiveHashMismatch)
}

// TestOpenCatalogArchiveRacingReRegisterIsAVersionChange: the row read sees
// h1, the object already holds h2, and the re-read proves the row moved —
// the client gets a version conflict, not a hash mismatch and never h2 bytes
// served against an h1 precondition.
func TestOpenCatalogArchiveRacingReRegisterIsAVersionChange(t *testing.T) {
	fx := newArchiveFixture(t, "file:archive-race?mode=memory&cache=shared")
	ctx := context.Background()
	h1 := buildSkillArchiveBytes(t, "C1", "Version one.", nil)
	h2 := buildSkillArchiveBytes(t, "C1", "Version two.", nil)
	cat := fx.registerCatalog(t, 1001, h1)

	h1Sum, h2Sum := sha256Hex(h1), sha256Hex(h2)
	swapped := false
	fx.fs.beforeGet = func() {
		if swapped {
			return
		}
		swapped = true
		// The racing re-register commits before the bytes are read back.
		_, err := fx.svc.RegisterCatalogFromArchive(ctx, 1001, h2)
		require.NoError(t, err)
		fx.fs.replaceObject(cat.BundleRef, h2)
	}

	_, err := fx.svc.OpenCatalogArchive(ctx, 1001, cat.ID, h1Sum)
	require.ErrorIs(t, err, ErrCatalogVersionChanged)
	_ = h2Sum
}

// TestOpenCatalogArchiveRacingReRegisterDoesNotPoisonPinnedInstalls is the
// M-1 regression (BND-007/VAL-213): a VAL-210 race interleaved between the
// row read and the object read must not leave the new version's bytes
// cached under the old version's digest — installs pinned to h1 keep
// reading the h1 tree afterwards.
func TestOpenCatalogArchiveRacingReRegisterDoesNotPoisonPinnedInstalls(t *testing.T) {
	fx := newArchiveFixture(t, "file:archive-cache-poison?mode=memory&cache=shared")
	ctx := context.Background()
	h1 := buildSkillArchiveBytes(t, "C1", "Version one.", map[string][]byte{
		"tree/version.txt": []byte("h1 tree"),
	})
	h2 := buildSkillArchiveBytes(t, "C1", "Version two, same skill.", map[string][]byte{
		"tree/version.txt": []byte("h2 tree"),
	})
	cat := fx.registerCatalog(t, 1001, h1)
	h1Sum, h2Sum := sha256Hex(h1), sha256Hex(h2)
	require.NotEqual(t, h1Sum, h2Sum)

	// A sandbox install still pinned to h1 under its own immutable copy —
	// exactly what the re-register pinning hands installs serving the old
	// tree. Its digest is h1, its reference is not the catalog's.
	pinnedRef := fx.fs.putRaw("file://pinned/h1-copy.zip", h1)
	require.NoError(t, fx.repo.CreateSkill(ctx, &types.TenantSkillEntity{
		ID: "sk-pinned", TenantID: 1001, SandboxConfigID: "cfg-a",
		CatalogID: cat.ID, Name: "C1", BundleRef: pinnedRef, BundleSHA256: h1Sum,
		Status: types.SkillStatusReady, Enabled: true,
	}))

	// Interleave the race: the catalog row read observes h1, then the
	// re-register commits (row h2 under a new reference) and the catalog's
	// own object is overwritten in place before the bytes are read. A
	// cache that pins the fetched h2 bytes under tenant:h1 would serve the
	// wrong tree to the pinned install afterwards.
	swapped := false
	fx.fs.beforeGet = func() {
		if swapped {
			return
		}
		swapped = true
		_, err := fx.svc.RegisterCatalogFromArchive(ctx, 1001, h2)
		require.NoError(t, err)
		fx.fs.replaceObject(cat.BundleRef, h2)
	}
	_, err := fx.svc.OpenCatalogArchive(ctx, 1001, cat.ID, h1Sum)
	require.ErrorIs(t, err, ErrCatalogVersionChanged)

	// After the race the pinned install must still read the h1 tree — from
	// its own object and the intact cache entry, never from h2 bytes.
	fx.fs.beforeGet = nil
	body, err := fx.svc.ReadSkillFile(ctx, 1001, "cfg-a", "sk-pinned", "tree/version.txt")
	require.NoError(t, err)
	require.Equal(t, "h1 tree", body.Content)
	files, err := fx.svc.ListSkillFiles(ctx, 1001, "cfg-a", "sk-pinned")
	require.NoError(t, err)
	require.Equal(t, []string{"SKILL.md", "tree/version.txt"}, skillFilePaths(files))

	// Sanity: the current catalog archive really is h2 now.
	archive, err := fx.svc.OpenCatalogArchive(ctx, 1001, cat.ID, h2Sum)
	require.NoError(t, err)
	require.True(t, bytesEqual(h2, archive.Archive))
}

func skillFilePaths(entries []SkillFileEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}

// TestOpenCatalogArchiveHonoursContextDeadline injects a slow storage read
// and requires the failure to surface promptly as not-ready rather than
// hanging or returning mismatched bytes.
func TestOpenCatalogArchiveHonoursContextDeadline(t *testing.T) {
	fx := newArchiveFixture(t, "file:archive-timeout?mode=memory&cache=shared")
	ctx := context.Background()
	h1 := buildSkillArchiveBytes(t, "C1", "Slow object.", nil)
	cat := fx.registerCatalog(t, 1001, h1)
	fx.fs.blockFor = 500 * time.Millisecond

	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := fx.svc.OpenCatalogArchive(deadline, 1001, cat.ID, sha256Hex(h1))
	require.ErrorIs(t, err, ErrCatalogArchiveNotReady)
	require.Less(t, time.Since(start), 450*time.Millisecond)
}

// TestOpenCatalogArchiveNeverExposesInternalRef: the archive view carries
// bytes, digest and a safe suggested filename — the storage reference stays
// internal.
func TestOpenCatalogArchiveNeverExposesInternalRef(t *testing.T) {
	fx := newArchiveFixture(t, "file:archive-ref-privacy?mode=memory&cache=shared")
	ctx := context.Background()
	h1 := buildSkillArchiveBytes(t, "C1 技能/包", "Odd name.", nil)
	cat := fx.registerCatalog(t, 1001, h1)
	archive, err := fx.svc.OpenCatalogArchive(ctx, 1001, cat.ID, sha256Hex(h1))
	require.NoError(t, err)
	require.NotContains(t, archive.FileName, "/")
	require.NotContains(t, archive.FileName, "..")
	require.Contains(t, archive.FileName, ".zip")
}

func bytesEqual(a, b []byte) bool {
	return sha256.Sum256(a) == sha256.Sum256(b)
}

// requireCatalogNotFound asserts the shared not-found shape the HTTP layer
// maps to 404 RESOURCE_NOT_ACCESSIBLE.
func requireCatalogNotFound(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	appErr, ok := apperrors.IsAppError(err)
	require.True(t, ok, "expected an AppError, got %v", err)
	require.Equal(t, 404, appErr.HTTPCode)
}
