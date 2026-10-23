-- Migration: 000133_drop_skill_release
-- WORK-RESOURCES-001 / ADR-008 / DATA-001: remove the legacy skill release
-- publication ledger. The new distribution path serves the original catalog
-- archive directly (GET /api/v1/skills/catalog/:id/archive); nothing writes
-- or reads skill_release anymore. This forward migration only drops
-- skill_release and the indexes owned by that table. tenant_skill_catalog,
-- tenant_skills, the MCP tables and every identity/audit table are left
-- untouched, and no historical migration is rewritten.
--
-- Preconditions enforced operationally (BND-011): the old release writes are
-- stopped, and a verified backup of this table's schema, rows and
-- archive_ref list exists before this runs. The companion tool
-- internal/database (BackupSkillReleaseTable / RestoreSkillReleaseTable)
-- produces and rehearses that backup.

BEGIN;

-- Owned indexes first: all four belong exclusively to skill_release.
DROP INDEX IF EXISTS idx_skill_release_tenant_created;
DROP INDEX IF EXISTS idx_skill_release_idempotency_key;
DROP INDEX IF EXISTS idx_skill_release_bundle_sha256;
DROP INDEX IF EXISTS idx_skill_release_catalog_version;

DROP TABLE IF EXISTS skill_release;

COMMIT;
