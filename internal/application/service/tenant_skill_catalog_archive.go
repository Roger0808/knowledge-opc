package service

import (
	"context"
	"errors"
	"strings"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
)

// Sentinel failures of OpenCatalogArchive. The HTTP layer maps them onto the
// INT-105 resource codes: a missing or invisible catalog is the shared
// not-found shape, an absent or unreadable object is not ready, a stale
// expected digest is a version conflict, and stored bytes that no longer
// hash to what the authoritative catalog row records are a hash mismatch.
var (
	ErrCatalogArchiveNotReady     = errors.New("catalog archive is not ready")
	ErrCatalogVersionChanged      = errors.New("catalog archive version changed")
	ErrCatalogArchiveHashMismatch = errors.New("catalog archive hash mismatch")
)

// CatalogArchive is the authoritative original bundle of one live catalog
// row. Archive carries the stored bytes verbatim — they are the bytes the
// workspace uploaded, never re-packed, never patched with a manifest.
type CatalogArchive struct {
	Archive []byte
	SHA256  string
	// FileName is the suggested download name; it carries no storage path.
	FileName string
}

// OpenCatalogArchive returns the authoritative original archive of one live
// catalog definition, strictly resolved by (tenant, catalog id).
//
// The lookup deliberately does NOT go through resolveCatalog: the catalog
// table is the only authority for what a catalog is, and resolving through
// the install projection would resurrect bytes for definitions the workspace
// deleted. Nor does it fall back to install rows or any release ledger: the
// bytes come from the BundleRef the catalog row itself names and nowhere
// else.
//
// Three digests line up before anything is returned:
//
//   - expected is the caller's precondition (the sha256 from the listing the
//     user confirmed). Against the authoritative row it must match or the
//     download is refused as a version conflict.
//   - current is the digest recorded on the catalog row.
//   - actual is recomputed here from the stored bytes. It must equal current
//     or the object no longer matches the metadata that names it.
//
// A racing re-register between the row read and the byte read is resolved by
// re-reading the row once: if the row moved, the answer is a version
// conflict. trySkillBundle refuses to cache bytes that do not hash to the
// key's digest, so a poisoned entry can never serve the new version's tree
// to installs pinned to the old one (BND-007/VAL-213); what remains for this
// call is to classify the failure honestly.
func (s *TenantSkillService) OpenCatalogArchive(
	ctx context.Context, tenantID uint64, catalogID, expectedSHA256 string,
) (*CatalogArchive, error) {
	catalog, err := s.strictCatalog(ctx, tenantID, catalogID)
	if err != nil {
		return nil, err
	}
	current := strings.TrimSpace(catalog.BundleSHA256)
	ref := strings.TrimSpace(catalog.BundleRef)
	if ref == "" || current == "" {
		return nil, ErrCatalogArchiveNotReady
	}
	expected := strings.TrimSpace(expectedSHA256)
	if expected != "" && !strings.EqualFold(expected, current) {
		return nil, ErrCatalogVersionChanged
	}
	row := &types.TenantSkillEntity{
		Name: catalog.Name, BundleRef: catalog.BundleRef, BundleSHA256: current,
	}
	archive, ok := s.trySkillBundle(ctx, tenantID, row)
	if !ok || len(archive) == 0 {
		// The fetch failed or the bytes do not hash to the row's digest.
		// Re-read the row once to tell the race from the corruption from
		// the outage. A failed re-read here does not change the answer:
		// the archive is unavailable either way, and a deadline or outage
		// on the fetch path is ARCHIVE_NOT_READY, not a server fault.
		moved, moveErr := s.catalogSHAChanged(ctx, tenantID, catalogID, current)
		if moveErr == nil {
			if moved {
				return nil, ErrCatalogVersionChanged
			}
			// The row is unchanged: probe the object once more to
			// distinguish a stored object whose bytes no longer hash to
			// the recorded digest (422) from one that is simply
			// unavailable right now (409). The probe is not cached, and
			// corruption is declared only when the fetched bytes
			// positively fail the row's digest.
			if probe, dlErr := s.downloadSkillBundle(ctx, tenantID, row); dlErr == nil &&
				!archiveMatchesSHA(probe, current) {
				return nil, ErrCatalogArchiveHashMismatch
			}
		}
		return nil, ErrCatalogArchiveNotReady
	}
	actual := skillArchiveSHA256(archive)
	if actual != current {
		// A cached or uncached mismatch with an unchanged row means the
		// stored object does not match the metadata naming it. Only a
		// positively-moved row downgrades this to a version conflict; a
		// re-read failure propagates as a server error.
		moved, moveErr := s.catalogSHAChanged(ctx, tenantID, catalogID, current)
		if moveErr != nil {
			return nil, moveErr
		}
		if moved {
			return nil, ErrCatalogVersionChanged
		}
		return nil, ErrCatalogArchiveHashMismatch
	}
	return &CatalogArchive{
		Archive:  archive,
		SHA256:   actual,
		FileName: catalogArchiveFileName(catalog, actual),
	}, nil
}

// strictCatalog resolves one catalog row by id in the caller's workspace and
// nothing else: no install-row projection, no name fallback, no synthetic
// definition. A missing row is indistinguishable from an invisible one.
func (s *TenantSkillService) strictCatalog(
	ctx context.Context, tenantID uint64, catalogID string,
) (*types.TenantSkillCatalogEntity, error) {
	id := strings.TrimSpace(catalogID)
	if id == "" {
		return nil, apperrors.NewNotFoundError("skill catalog is not accessible")
	}
	catalog, err := s.skills.GetCatalog(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, apperrors.NewNotFoundError("skill catalog is not accessible")
	}
	return catalog, nil
}

// catalogSHAChanged re-reads one catalog row and reports whether its
// authoritative digest differs from the one the first read observed.
func (s *TenantSkillService) catalogSHAChanged(
	ctx context.Context, tenantID uint64, catalogID, observedSHA string,
) (bool, error) {
	catalog, err := s.skills.GetCatalog(ctx, tenantID, catalogID)
	if err != nil {
		return false, err
	}
	if catalog == nil {
		// Deleted mid-flight: treat as moved so the caller surfaces a
		// version conflict rather than probing a definition that is gone.
		return true, nil
	}
	return strings.TrimSpace(catalog.BundleSHA256) != observedSHA, nil
}

// catalogArchiveFileName suggests a stable download name derived from the
// catalog identity and content digest, never from the storage reference.
func catalogArchiveFileName(catalog *types.TenantSkillCatalogEntity, sha256 string) string {
	name := "skill"
	if catalog != nil {
		name = catalogArchiveSafeName(catalog.Name)
	}
	digest := sha256
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return name + "-" + digest + ".zip"
}

// catalogArchiveSafeName keeps printable filename characters and collapses
// everything else, so a hostile skill name cannot smuggle path separators or
// header-breaking characters into Content-Disposition. Runs of dots are
// collapsed to one: the result can never contain a dot-dot pair at all.
func catalogArchiveSafeName(raw string) string {
	var b strings.Builder
	lastDash := false
	lastDot := false
	for _, r := range strings.TrimSpace(raw) {
		if r == '.' {
			if lastDot {
				continue
			}
			b.WriteByte('.')
			lastDot = true
			lastDash = false
			continue
		}
		lastDot = false
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
			continue
		}
		b.WriteRune(r)
		lastDash = false
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		return "skill"
	}
	if len(name) > 64 {
		name = strings.Trim(name[:64], "-.")
	}
	if name == "" {
		return "skill"
	}
	return name
}
