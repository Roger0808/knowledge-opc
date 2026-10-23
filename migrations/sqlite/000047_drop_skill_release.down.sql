-- Migration: 000047_drop_skill_release (SQLite down)
-- Recreates the legacy skill_release publication ledger as an EMPTY table
-- with the structure recorded by 000046_opc_integration (mirroring versioned
-- 000129). This structural down is not a data restore: recovering pre-drop
-- rows must go through the verified table backup (see internal/database
-- BackupSkillReleaseTable / RestoreSkillReleaseTable).

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
