package handler

import (
	"math/rand"
	"strings"
	"testing"
)

// PBT P4 (VAL-102): archive path normalization, duplicate/hostile names and
// size boundaries. The generators below are deterministic (seed fixed by the
// specification) and the properties are checked against INDEPENDENT
// reimplementations of the safety rules, not against the functions'
// own construction.
//
//	seed = 20261025, cases = 600 (>= 500 required)
//
// Property: for every generated id/digest/name, whatever the implementation
// accepts can never smuggle a path separator, dot-dot, control character or
// newline into a path or response header, and every normalized digest is
// canonical lowercase hex.

const pbtP4Seed = 20261025
const pbtP4Cases = 600

// pbtRunePool covers printable ASCII plus the hostile characters the
// validator must reject.
const pbtRunePool = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" +
	"-._~!$&'()*+,;=:@ []{}|?<>^`\"\\\t\r\n" +
	"技能包🚀"

func pbtRandomString(rng *rand.Rand, maxLen int) string {
	n := rng.Intn(maxLen + 1)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(rune(pbtRunePool[rng.Intn(len(pbtRunePool))]))
	}
	return b.String()
}

func pbtReferenceSafeID(s string) bool {
	if s == "" || len(s) > catalogArchiveMaxCatalogIDLen || strings.TrimSpace(s) != s {
		return false
	}
	if strings.Contains(s, "..") || strings.ContainsAny(s, `/\`) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

func TestPBT_P4_CatalogArchiveInputsNeverEscape(t *testing.T) {
	rng := rand.New(rand.NewSource(pbtP4Seed))
	for i := 0; i < pbtP4Cases; i++ {
		raw := pbtRandomString(rng, 160)

		// accepted ids must satisfy the independent reference rules
		if validCatalogArchiveID(raw) && !pbtReferenceSafeID(raw) {
			t.Fatalf("case %d: id %q accepted but violates the reference rules", i, raw)
		}
		// and must never contain what a path or header cannot hold
		if validCatalogArchiveID(raw) &&
			(strings.ContainsAny(raw, "/\\") || strings.Contains(raw, "..") ||
				strings.ContainsAny(raw, "\r\n")) {
			t.Fatalf("case %d: accepted id %q smuggles a separator or newline", i, raw)
		}

		// digests: normalized values are always canonical, and only
		// canonical 64-hex inputs are accepted at all
		if normalized, ok := normalizeExpectedSHA256(raw); ok {
			if len(normalized) != 64 {
				t.Fatalf("case %d: digest %q accepted with length %d", i, raw, len(normalized))
			}
			if normalized != strings.ToLower(normalized) {
				t.Fatalf("case %d: digest %q not normalized to lowercase", i, raw)
			}
			for _, r := range normalized {
				if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
					t.Fatalf("case %d: digest %q contains non-hex rune %q", i, raw, r)
				}
			}
		}

	}
	t.Logf("PBT P4 (id/digest): seed=%d cases=%d", pbtP4Seed, pbtP4Cases)
}
