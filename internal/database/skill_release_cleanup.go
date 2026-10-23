package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// WORK-RESOURCES-001 / ADR-008 / BND-011: backup and restore tooling for the
// legacy skill_release publication ledger that versioned migration 000133 and
// SQLite migration 000047 drop. The migration only removes the table; every
// rollback plan goes through this tool, which exports the schema shape, all
// rows and the archive_ref list into one checksum-manifested document so a
// restore can be rehearsed and verified (row count + row hash) before any
// real database is cleaned. Shared tables (tenant_skill_catalog,
// tenant_skills, mcp_invocations, mcp_invocation_decisions, identities) are
// never touched here, and object storage is never cleaned by this package.

// SkillReleaseTable names the legacy ledger this tool backs up and restores.
const SkillReleaseTable = "skill_release"

// SkillReleaseDialect selects the SQL dialect of the database at hand.
type SkillReleaseDialect string

const (
	SkillReleaseDialectPostgres SkillReleaseDialect = "postgres"
	SkillReleaseDialectSQLite   SkillReleaseDialect = "sqlite"
)

// skillReleaseColumns is the authoritative column list, identical to what
// versioned migration 000129 (PostgreSQL) and SQLite migration
// 000046_opc_integration created. The backup stores rows in exactly this
// order so a backup taken on one dialect restores on the other.
var skillReleaseColumns = []string{
	"id", "tenant_id", "catalog_id", "version", "bundle_sha256", "archive_ref",
	"manifest", "target_tenants", "target_roles", "idempotency_key", "status",
	"published_by", "published_at", "delisted_at", "created_at", "updated_at",
}

// skillReleasePostgresSchema recreates the empty ledger exactly as
// 000133_drop_skill_release.down.sql records it.
const skillReleasePostgresSchema = `
CREATE TABLE IF NOT EXISTS skill_release (
    id VARCHAR(36) PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id BIGINT NOT NULL,
    catalog_id VARCHAR(36) NOT NULL,
    version VARCHAR(64) NOT NULL,
    bundle_sha256 VARCHAR(64) NOT NULL,
    archive_ref TEXT NOT NULL DEFAULT '',
    manifest JSONB,
    target_tenants JSONB NOT NULL DEFAULT '[]'::jsonb,
    target_roles JSONB NOT NULL DEFAULT '[]'::jsonb,
    idempotency_key TEXT,
    status VARCHAR(16) NOT NULL DEFAULT 'published',
    published_by VARCHAR(64) NOT NULL DEFAULT '',
    published_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    delisted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_release_catalog_version
    ON skill_release (catalog_id, version);
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_release_bundle_sha256
    ON skill_release (bundle_sha256);
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_release_idempotency_key
    ON skill_release (idempotency_key)
    WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';
CREATE INDEX IF NOT EXISTS idx_skill_release_tenant_created
    ON skill_release (tenant_id, created_at DESC);
`

// skillReleaseSQLiteSchema recreates the empty ledger exactly as
// 000047_drop_skill_release.down.sql records it.
const skillReleaseSQLiteSchema = `
CREATE TABLE IF NOT EXISTS skill_release (
    id VARCHAR(36) PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    catalog_id VARCHAR(36) NOT NULL,
    version VARCHAR(64) NOT NULL,
    bundle_sha256 VARCHAR(64) NOT NULL,
    archive_ref TEXT NOT NULL DEFAULT '',
    manifest TEXT,
    target_tenants TEXT NOT NULL DEFAULT '[]',
    target_roles TEXT NOT NULL DEFAULT '[]',
    idempotency_key TEXT,
    status VARCHAR(16) NOT NULL DEFAULT 'published',
    published_by VARCHAR(64) NOT NULL DEFAULT '',
    published_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    delisted_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_release_catalog_version
    ON skill_release (catalog_id, version);
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_release_bundle_sha256
    ON skill_release (bundle_sha256);
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_release_idempotency_key
    ON skill_release (idempotency_key)
    WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';
CREATE INDEX IF NOT EXISTS idx_skill_release_tenant_created
    ON skill_release (tenant_id, created_at DESC);
`

// SkillReleaseBackup is one portable, checksum-manifested export of the
// legacy ledger. Rows carry no secrets: the manifest column is the validated
// opc-client.json document the release was published with, and archive_ref is
// the opaque object reference — nothing here is a token or a signed URL.
type SkillReleaseBackup struct {
	Dialect     SkillReleaseDialect `json:"dialect"`
	Columns     []string            `json:"columns"`
	Rows        [][]any             `json:"rows"`
	ArchiveRefs []string            `json:"archive_refs"`
	// RowChecksum is the SHA-256 of the canonical encoding of Rows and
	// ArchiveRefs; VerifySkillReleaseBackup recomputes it after every read
	// and every restore.
	RowChecksum string `json:"row_checksum"`
}

// SkillReleaseTableExists reports whether the legacy ledger still exists.
func SkillReleaseTableExists(ctx context.Context, db *sql.DB, dialect SkillReleaseDialect) (bool, error) {
	var found bool
	var err error
	switch dialect {
	case SkillReleaseDialectSQLite:
		err = db.QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", SkillReleaseTable).Scan(&found)
		if err != nil {
			return false, err
		}
	case SkillReleaseDialectPostgres:
		err = db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name=$1)", SkillReleaseTable).Scan(&found)
		if err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("unsupported dialect %q", dialect)
	}
	return found, nil
}

// BackupSkillReleaseTable exports the full legacy ledger. A database that
// already cleaned the table backs up as zero rows — the checksum still
// verifies, and restores from it recreate only the empty structure.
func BackupSkillReleaseTable(
	ctx context.Context, db *sql.DB, dialect SkillReleaseDialect,
) (*SkillReleaseBackup, error) {
	exists, err := SkillReleaseTableExists(ctx, db, dialect)
	if err != nil {
		return nil, err
	}
	backup := &SkillReleaseBackup{
		Dialect:     dialect,
		Columns:     append([]string(nil), skillReleaseColumns...),
		Rows:        [][]any{},
		ArchiveRefs: []string{},
	}
	if !exists {
		backup.RowChecksum = skillReleaseRowsChecksum(backup.Rows, backup.ArchiveRefs)
		return backup, nil
	}

	columnList := strings.Join(skillReleaseColumns, ", ")
	query := fmt.Sprintf("SELECT %s FROM %s ORDER BY id", columnList, SkillReleaseTable)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read %s rows: %w", SkillReleaseTable, err)
	}
	defer func() { _ = rows.Close() }()

	refs := map[string]struct{}{}
	for rows.Next() {
		raw := make([]any, len(skillReleaseColumns))
		ptrs := make([]any, len(skillReleaseColumns))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scan %s row: %w", SkillReleaseTable, err)
		}
		values := make([]any, len(raw))
		for i, v := range raw {
			values[i], err = normaliseSkillReleaseValue(v)
			if err != nil {
				return nil, fmt.Errorf("normalise %s.%s: %w", SkillReleaseTable, skillReleaseColumns[i], err)
			}
		}
		backup.Rows = append(backup.Rows, values)
		if i := columnIndex("archive_ref"); i >= 0 {
			if ref, ok := values[i].(string); ok && strings.TrimSpace(ref) != "" {
				refs[ref] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for ref := range refs {
		backup.ArchiveRefs = append(backup.ArchiveRefs, ref)
	}
	sort.Strings(backup.ArchiveRefs)
	backup.RowChecksum = skillReleaseRowsChecksum(backup.Rows, backup.ArchiveRefs)
	return backup, nil
}

func columnIndex(name string) int {
	for i, c := range skillReleaseColumns {
		if c == name {
			return i
		}
	}
	return -1
}

// normaliseSkillReleaseValue turns driver-specific values into stable JSON
// shapes: times become RFC 3339 strings, byte slices become strings, and
// everything else passes through.
func normaliseSkillReleaseValue(v any) (any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano), nil
	case []byte:
		return string(t), nil
	case string, int64, float64, bool:
		return t, nil
	default:
		return fmt.Sprintf("%v", t), nil
	}
}

// skillReleaseRowsChecksum is the canonical digest over rows and refs.
// encoding/json marshals nested string slices deterministically, so the same
// logical backup always yields the same checksum.
func skillReleaseRowsChecksum(rows [][]any, refs []string) string {
	payload, _ := json.Marshal(struct {
		Rows [][]any  `json:"rows"`
		Refs []string `json:"refs"`
	}{Rows: rows, Refs: refs})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// VerifySkillReleaseBackup recomputes the manifest checksum and returns an
// error when the document was altered or corrupted.
func VerifySkillReleaseBackup(backup *SkillReleaseBackup) error {
	if backup == nil {
		return errors.New("skill release backup is nil")
	}
	if got := skillReleaseRowsChecksum(backup.Rows, backup.ArchiveRefs); got != backup.RowChecksum {
		return fmt.Errorf("skill release backup checksum mismatch: manifest %s, recomputed %s",
			backup.RowChecksum, got)
	}
	return nil
}

// WriteSkillReleaseBackup persists one manifest-checked document and returns
// its path. The document is the only artifact a rollback needs.
func WriteSkillReleaseBackup(dir string, backup *SkillReleaseBackup) (string, error) {
	if err := VerifySkillReleaseBackup(backup); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "skill_release_backup.json")
	data, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ReadSkillReleaseBackup loads a backup document and verifies its checksum.
func ReadSkillReleaseBackup(path string) (*SkillReleaseBackup, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var backup SkillReleaseBackup
	if err := json.Unmarshal(data, &backup); err != nil {
		return nil, err
	}
	if err := VerifySkillReleaseBackup(&backup); err != nil {
		return nil, err
	}
	return &backup, nil
}

// RestoreSkillReleaseTable recreates the empty ledger (structure only) and
// inserts every backed-up row inside one transaction, so a mid-restore
// failure rolls back instead of leaving a half-populated ledger behind. It
// returns the number of rows inserted. The table is created only when
// missing: restoring over an intact ledger never touches its data, which is
// the shared-reference protection the cleanup protocol requires.
func RestoreSkillReleaseTable(
	ctx context.Context, db *sql.DB, dialect SkillReleaseDialect, backup *SkillReleaseBackup,
) (int, error) {
	if backup == nil {
		return 0, errors.New("skill release backup is nil")
	}
	if err := VerifySkillReleaseBackup(backup); err != nil {
		return 0, err
	}
	schema := skillReleaseSQLiteSchema
	placeholder := func(i int) string { return "?" }
	if dialect == SkillReleaseDialectPostgres {
		schema = skillReleasePostgresSchema
		placeholder = func(i int) string { return fmt.Sprintf("$%d", i+1) }
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return 0, fmt.Errorf("recreate %s structure: %w", SkillReleaseTable, err)
	}

	if len(backup.Rows) > 0 {
		if len(backup.Columns) != len(skillReleaseColumns) {
			return 0, fmt.Errorf("backup has %d columns, want %d", len(backup.Columns), len(skillReleaseColumns))
		}
		marks := make([]string, len(skillReleaseColumns))
		for i := range marks {
			marks[i] = placeholder(i)
		}
		stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
			SkillReleaseTable, strings.Join(skillReleaseColumns, ", "), strings.Join(marks, ", "))
		for _, row := range backup.Rows {
			if len(row) != len(skillReleaseColumns) {
				return 0, fmt.Errorf("backup row has %d values, want %d", len(row), len(skillReleaseColumns))
			}
			if _, err := tx.ExecContext(ctx, stmt, row...); err != nil {
				return 0, fmt.Errorf("restore %s row: %w", SkillReleaseTable, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(backup.Rows), nil
}
