package service

import (
	"context"
	"io"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

// PBT P4 (VAL-102), filename-sanitizer half: the suggested download name of a
// catalog archive is built from the workspace-controlled skill name, so it
// must be impossible — over a deterministic generated corpus — to smuggle a
// path separator, dot-dot, newline or unbounded length into it. The
// reference sanitizer below is an independent reimplementation of the rules,
// not a call of the implementation under test.
//
//	seed = 20261025, cases = 600 (>= 500 required)

const pbtP4NameSeed = 20261025
const pbtP4NameCases = 600

const pbtNameRunePool = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" +
	"-._~!$&'()*+,;=:@ []{}|?<>^`\"\\\t\r\n" +
	"技能包🚀"

func pbtRandomName(rng *rand.Rand, maxLen int) string {
	n := rng.Intn(maxLen + 1)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(rune(pbtNameRunePool[rng.Intn(len(pbtNameRunePool))]))
	}
	return b.String()
}

func pbtReferenceName(raw string) string {
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

func TestPBT_P4_DownloadNameNeverEscapes(t *testing.T) {
	rng := rand.New(rand.NewSource(pbtP4NameSeed))
	for i := 0; i < pbtP4NameCases; i++ {
		raw := pbtRandomName(rng, 200)
		got := catalogArchiveSafeName(raw)
		want := pbtReferenceName(raw)
		if got != want {
			t.Fatalf("case %d: safe name for %q = %q, reference %q", i, raw, got, want)
		}
		if strings.ContainsAny(got, "/\\\r\n") {
			t.Fatalf("case %d: download name %q from %q contains a path separator or newline", i, got, raw)
		}
		if strings.Contains(got, "..") {
			t.Fatalf("case %d: download name %q from %q contains dot-dot", i, got, raw)
		}
		if len(got) > 64 {
			t.Fatalf("case %d: download name %q exceeds the 64-byte bound", i, got)
		}
		if strings.Trim(got, "-.") != got {
			t.Fatalf("case %d: download name %q carries leading/trailing dashes or dots", i, got)
		}
	}
	t.Logf("PBT P4 (download name): seed=%d cases=%d", pbtP4NameSeed, pbtP4NameCases)
}

// repeatReader yields pattern bytes forever; wrapped in io.LimitReader it
// materialises only what the consumer reads, so the size-boundary case does
// not allocate the full corpus ahead of time.
type repeatReader struct {
	pattern []byte
	pos     int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.pattern[r.pos%len(r.pattern)]
		r.pos++
	}
	return len(p), nil
}

// TestPBT_P4_ArchiveSizeBoundary pins the VAL-102 P4 size boundary of the
// archive download path (downloadSkillBundle's LimitReader gate, which the
// catalog archive route reuses):
//
//	seed = 20261025, cases = 600 in-limit + 2 boundary cases
//
// Property: every generated archive within the limit downloads byte-for-byte;
// an object exactly at the limit is accepted; one byte over the limit is
// refused outright — never truncated, never cached.
func TestPBT_P4_ArchiveSizeBoundary(t *testing.T) {
	rng := rand.New(rand.NewSource(pbtP4NameSeed))
	for i := 0; i < pbtP4NameCases; i++ {
		size := 1 + rng.Intn(1<<20)
		archive := make([]byte, size)
		rng.Read(archive)
		fs := &archiveFileService{objects: map[string][]byte{
			"file://in-limit.zip": archive,
		}}
		svc := NewTenantSkillService(catalogTestRepo(t,
			"file:pbt-size-in?mode=memory&cache=shared"),
			nil, &archiveStorageResolver{fs: fs}, nil, nil, nil, nil, nil, nil, nil, nil, nil,
			HostSandboxManager{})
		got, err := svc.downloadSkillBundle(context.Background(), 1001,
			&types.TenantSkillEntity{Name: "c1", BundleRef: "file://in-limit.zip"})
		require.NoError(t, err, "case %d: %d-byte archive must download", i, size)
		require.True(t, bytesEqual(archive, got), "case %d: downloaded bytes must be exact", i)
	}

	// The two boundary cases share one service; the object table carries a
	// reference whose reader yields exactly the limit (accepted) and one
	// whose reader yields the limit plus one byte (refused).
	limit := int64(maxSkillBundleTotalBytes)
	fs := &archiveFileService{
		objects: map[string][]byte{},
		rawReaders: map[string]io.ReadCloser{
			"file://at-limit.zip":   io.NopCloser(io.LimitReader(&repeatReader{pattern: []byte("wr001-boundary")}, limit)),
			"file://over-limit.zip": io.NopCloser(io.LimitReader(&repeatReader{pattern: []byte("wr001-boundary")}, limit+1)),
		},
	}
	svc := NewTenantSkillService(catalogTestRepo(t,
		"file:pbt-size-boundary?mode=memory&cache=shared"),
		nil, &archiveStorageResolver{fs: fs}, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		HostSandboxManager{})

	atLimit, err := svc.downloadSkillBundle(context.Background(), 1001,
		&types.TenantSkillEntity{Name: "c1", BundleRef: "file://at-limit.zip"})
	require.NoError(t, err, "an object exactly at the limit downloads")
	require.Equal(t, int(limit), len(atLimit))

	_, err = svc.downloadSkillBundle(context.Background(), 1001,
		&types.TenantSkillEntity{Name: "c1", BundleRef: "file://over-limit.zip"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "larger than the upload limit")

	t.Logf("PBT P4 (size boundary): seed=%d cases=%d+%d", pbtP4NameSeed, pbtP4NameCases, 2)
}
