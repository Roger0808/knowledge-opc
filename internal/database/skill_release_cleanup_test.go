package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	sqlite3migrate "github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/stretchr/testify/require"
)

// openMigratedSQLite opens a SQLite database and migrates it to exactly
// version `version` using the repo's migration files, so a test can observe
// the database just before and just after one migration step.
func openMigratedSQLite(t *testing.T, repoRoot, dbPath string, version uint) (*sql.DB, *migrate.Migrate) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	driver, err := sqlite3migrate.WithInstance(sqlDB, &sqlite3migrate.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithDatabaseInstance("file://migrations/sqlite", "sqlite3", driver)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })
	require.NoError(t, m.Migrate(version))
	return sqlDB, m
}

// representativeSkillReleaseRows builds two legacy release rows with distinct
// content and archive references, standing in for the pre-cleanup存量 rows
// of VAL-217.
func representativeSkillReleaseRows() [][]any {
	return [][]any{
		{
			"rel-1", int64(1001), "cat-c1", "1.0.0",
			"1111111111111111111111111111111111111111111111111111111111111111",
			"file://tenant-skills/releases/rel-1.zip",
			`{"skill_id":"cat-c1","version":"1.0.0","sha256":"11","exec_mode":"sandbox"}`,
			"[]", "[]", nil, "published", "u1",
			"2026-10-01T10:00:00Z", nil, "2026-10-01T10:00:00Z", "2026-10-01T10:00:00Z",
		},
		{
			"rel-2", int64(1001), "cat-c1", "2.0.0",
			"2222222222222222222222222222222222222222222222222222222222222222",
			"file://tenant-skills/releases/rel-2.zip",
			`{"skill_id":"cat-c1","version":"2.0.0","sha256":"22","exec_mode":"sandbox"}`,
			"[1002]", `["viewer"]`, "idem-2", "delisted", "u1",
			"2026-10-02T10:00:00Z", "2026-10-03T10:00:00Z", "2026-10-02T10:00:00Z", "2026-10-03T10:00:00Z",
		},
	}
}

func insertSkillReleaseRows(t *testing.T, db *sql.DB, rows [][]any) {
	t.Helper()
	insertSkillReleaseRowsDialect(t, db, rows, false)
}

func insertSkillReleaseRowsDialect(t *testing.T, db *sql.DB, rows [][]any, postgres bool) {
	t.Helper()
	marks := make([]string, 16)
	for i := range marks {
		if postgres {
			marks[i] = fmt.Sprintf("$%d", i+1)
		} else {
			marks[i] = "?"
		}
	}
	stmt := "INSERT INTO skill_release (id, tenant_id, catalog_id, version, bundle_sha256, archive_ref, manifest, target_tenants, target_roles, idempotency_key, status, published_by, published_at, delisted_at, created_at, updated_at) VALUES (" +
		strings.Join(marks, ", ") + ")"
	for _, row := range rows {
		_, err := db.Exec(stmt, row...)
		require.NoError(t, err)
	}
}

func countTableRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&n))
	return n
}

// VAL-217 empty-database path: a database already migrated past the cleanup
// backs up as zero rows with a verifiable checksum, and the document
// round-trips through disk.
func TestSkillReleaseBackupEmptyDatabase(t *testing.T) {
	repoRoot := sqliteRepoRoot(t)
	chdirAndRestore(t, repoRoot)
	ctx := context.Background()

	db, _ := openMigratedSQLite(t, repoRoot, filepath.Join(t.TempDir(), "empty.db"), 47)
	defer func() { _ = db.Close() }()
	require.False(t, sqliteTableExists(t, db, SkillReleaseTable))
	// The MCP tables from the same combined migration must survive the
	// cleanup migration.
	require.True(t, sqliteTableExists(t, db, "mcp_invocations"))
	require.True(t, sqliteTableExists(t, db, "mcp_invocation_decisions"))

	backup, err := BackupSkillReleaseTable(ctx, db, SkillReleaseDialectSQLite)
	require.NoError(t, err)
	require.Empty(t, backup.Rows)
	require.Empty(t, backup.ArchiveRefs)
	require.NoError(t, VerifySkillReleaseBackup(backup))

	path, err := WriteSkillReleaseBackup(t.TempDir(), backup)
	require.NoError(t, err)
	loaded, err := ReadSkillReleaseBackup(path)
	require.NoError(t, err)
	require.Equal(t, backup.RowChecksum, loaded.RowChecksum)
}

// VAL-217 representative-rows path: back up a populated ledger, run the
// cleanup migration, restore from the backup, and verify row count, row hash
// and the archive_ref list. Shared tables keep their data throughout.
func TestSkillReleaseBackupRestoreRoundTrip(t *testing.T) {
	repoRoot := sqliteRepoRoot(t)
	chdirAndRestore(t, repoRoot)
	ctx := context.Background()

	// Migrate to 46 (before the cleanup), seed representative legacy rows.
	db, m := openMigratedSQLite(t, repoRoot, filepath.Join(t.TempDir(), "stock.db"), 46)
	require.True(t, sqliteTableExists(t, db, SkillReleaseTable))
	rows := representativeSkillReleaseRows()
	insertSkillReleaseRows(t, db, rows)

	backup, err := BackupSkillReleaseTable(ctx, db, SkillReleaseDialectSQLite)
	require.NoError(t, err)
	require.Len(t, backup.Rows, 2)
	require.Equal(t, []string{
		"file://tenant-skills/releases/rel-1.zip",
		"file://tenant-skills/releases/rel-2.zip",
	}, backup.ArchiveRefs)
	require.NoError(t, VerifySkillReleaseBackup(backup))
	backupPath, err := WriteSkillReleaseBackup(t.TempDir(), backup)
	require.NoError(t, err)
	originalChecksum := backup.RowChecksum

	// MCP rows seeded alongside must still be there after the cleanup.
	_, err = db.Exec(
		"INSERT INTO mcp_invocations (invocation_id, tenant_id, subject, service_id, tool_name, args_digest, status) " +
			"VALUES ('inv-1', 1001, 'u1', 'svc-1', 'tool.a', 'digest', 'completed')")
	require.NoError(t, err)

	// The cleanup migration drops the ledger and nothing else.
	require.NoError(t, m.Migrate(47))
	require.False(t, sqliteTableExists(t, db, SkillReleaseTable))
	require.True(t, sqliteTableExists(t, db, "mcp_invocations"))
	require.Equal(t, 1, countTableRows(t, db, "mcp_invocations"))
	require.True(t, sqliteTableExists(t, db, "tenant_skill_catalog"))

	// Restore from the verified backup and re-verify count + hash + refs.
	loaded, err := ReadSkillReleaseBackup(backupPath)
	require.NoError(t, err)
	restored, err := RestoreSkillReleaseTable(ctx, db, SkillReleaseDialectSQLite, loaded)
	require.NoError(t, err)
	require.Equal(t, 2, restored)
	require.True(t, sqliteTableExists(t, db, SkillReleaseTable))
	require.Equal(t, 2, countTableRows(t, db, SkillReleaseTable))

	after, err := BackupSkillReleaseTable(ctx, db, SkillReleaseDialectSQLite)
	require.NoError(t, err)
	require.Equal(t, originalChecksum, after.RowChecksum, "restored rows must hash exactly like the backup")
	require.Equal(t, backup.ArchiveRefs, after.ArchiveRefs)

	// A second restore over the intact ledger must fail without touching data.
	_, err = RestoreSkillReleaseTable(ctx, db, SkillReleaseDialectSQLite, loaded)
	require.Error(t, err)
	require.Equal(t, 2, countTableRows(t, db, SkillReleaseTable))
}

// The structural down recreates an EMPTY ledger; data recovery only happens
// through the backup tool — down alone must not present itself as a restore.
func TestSkillReleaseDownRecreatesEmptyTableOnly(t *testing.T) {
	repoRoot := sqliteRepoRoot(t)
	chdirAndRestore(t, repoRoot)

	db, m := openMigratedSQLite(t, repoRoot, filepath.Join(t.TempDir(), "down.db"), 47)
	require.False(t, sqliteTableExists(t, db, SkillReleaseTable))
	require.NoError(t, m.Migrate(46))
	require.True(t, sqliteTableExists(t, db, SkillReleaseTable))
	require.Equal(t, 0, countTableRows(t, db, SkillReleaseTable),
		"the structural down must recreate an empty ledger, not recover rows")
}

// A restore that hits a constraint failure rolls back completely instead of
// leaving a half-populated ledger (transaction-failure recovery, BND-011).
func TestSkillReleaseRestoreFailureRollsBack(t *testing.T) {
	repoRoot := sqliteRepoRoot(t)
	chdirAndRestore(t, repoRoot)
	ctx := context.Background()

	db, _ := openMigratedSQLite(t, repoRoot, filepath.Join(t.TempDir(), "txn.db"), 47)
	defer func() { _ = db.Close() }()

	backup, err := BackupSkillReleaseTable(ctx, db, SkillReleaseDialectSQLite)
	require.NoError(t, err)
	// Two rows sharing one primary key cannot both insert.
	backup.Rows = append(backup.Rows, representativeSkillReleaseRows()[0], representativeSkillReleaseRows()[0])
	backup.RowChecksum = ""
	backup.RowChecksum = skillReleaseRowsChecksum(backup.Rows, backup.ArchiveRefs)

	_, err = RestoreSkillReleaseTable(ctx, db, SkillReleaseDialectSQLite, backup)
	require.Error(t, err)
	// The whole restore is one transaction: a constraint failure rolls the
	// structure back with the rows, leaving nothing half-populated behind.
	if sqliteTableExists(t, db, SkillReleaseTable) {
		require.Equal(t, 0, countTableRows(t, db, SkillReleaseTable),
			"a failed restore must roll back every inserted row")
	}
}

// A tampered backup document is refused before anything executes.
func TestSkillReleaseBackupTamperEvident(t *testing.T) {
	backup := &SkillReleaseBackup{
		Dialect:     SkillReleaseDialectSQLite,
		Columns:     append([]string(nil), skillReleaseColumns...),
		Rows:        representativeSkillReleaseRows(),
		ArchiveRefs: []string{"file://tenant-skills/releases/rel-1.zip"},
	}
	backup.RowChecksum = skillReleaseRowsChecksum(backup.Rows, backup.ArchiveRefs)
	require.NoError(t, VerifySkillReleaseBackup(backup))

	backup.Rows[0][4] = "tampered"
	require.Error(t, VerifySkillReleaseBackup(backup))
}

// PostgreSQL counterpart of the round trip, gated on a disposable database
// exactly like the other migration tests (WEKNORA_MIGRATION_TEST_POSTGRES_DSN).
// The full versioned migration chain cannot run on a plain PostgreSQL here
// (000002 needs pg_search), so this test builds the legacy table from the
// same canonical schema the cleanup tool restores, and exercises backup,
// drop, restore and re-verification against the real engine.
func TestSkillReleaseBackupRestorePostgreSQL(t *testing.T) {
	dsn := os.Getenv("WEKNORA_MIGRATION_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set WEKNORA_MIGRATION_TEST_POSTGRES_DSN to run PostgreSQL cleanup tests")
	}
	ctx := context.Background()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Clean slate: whatever a previous run left behind goes away.
	_, err = db.ExecContext(ctx, "DROP TABLE IF EXISTS skill_release")
	require.NoError(t, err)

	// Recreate the empty legacy ledger with the tool itself, then seed two
	// representative rows through the public column order.
	empty, err := BackupSkillReleaseTable(ctx, db, SkillReleaseDialectPostgres)
	require.NoError(t, err)
	_, err = RestoreSkillReleaseTable(ctx, db, SkillReleaseDialectPostgres, empty)
	require.NoError(t, err)
	insertSkillReleaseRowsDialect(t, db, representativeSkillReleaseRows(), true)

	backup, err := BackupSkillReleaseTable(ctx, db, SkillReleaseDialectPostgres)
	require.NoError(t, err)
	require.Len(t, backup.Rows, 2)
	require.NoError(t, VerifySkillReleaseBackup(backup))
	backupPath, err := WriteSkillReleaseBackup(t.TempDir(), backup)
	require.NoError(t, err)
	originalChecksum := backup.RowChecksum

	// The cleanup migration drops the ledger (simulated here with the same
	// DROP statements 000133 runs).
	_, err = db.ExecContext(ctx, `
		DROP INDEX IF EXISTS idx_skill_release_tenant_created;
		DROP INDEX IF EXISTS idx_skill_release_idempotency_key;
		DROP INDEX IF EXISTS idx_skill_release_bundle_sha256;
		DROP INDEX IF EXISTS idx_skill_release_catalog_version;
		DROP TABLE IF EXISTS skill_release;`)
	require.NoError(t, err)

	loaded, err := ReadSkillReleaseBackup(backupPath)
	require.NoError(t, err)
	restored, err := RestoreSkillReleaseTable(ctx, db, SkillReleaseDialectPostgres, loaded)
	require.NoError(t, err)
	require.Equal(t, 2, restored)

	after, err := BackupSkillReleaseTable(ctx, db, SkillReleaseDialectPostgres)
	require.NoError(t, err)
	require.Equal(t, originalChecksum, after.RowChecksum, "restored rows must hash exactly like the backup")
}
