-- Migration: 000129_skill_release
-- EXT-001 local skill release records. One row is an immutable, server-side
-- attested publication of a tenant_skill_catalog archive: the bytes are read
-- back from the file service at publish time, re-hashed, the manifest is
-- parsed and validated out of the bundle, and the attested bytes are copied
-- under the immutable per-release key tenant-skills/releases/<id>.zip before
-- anything is written. archive_ref names that copy, so a re-register or
-- delete of the catalog cannot rewrite what a published release serves.
CREATE TABLE IF NOT EXISTS skill_release (
    id VARCHAR(36) PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- tenant_id is the source workspace that owns the catalog definition.
    tenant_id BIGINT NOT NULL,
    catalog_id VARCHAR(36) NOT NULL,
    version VARCHAR(64) NOT NULL,
    -- bundle_sha256 is recomputed by the server from the stored archive
    -- bytes at publish time, never taken from the client.
    bundle_sha256 VARCHAR(64) NOT NULL,
    -- archive_ref names the immutable stored object; it is an opaque storage
    -- reference and is never rendered to clients as a usable path.
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

-- A catalog version is published once. Re-publishing identical bytes returns
-- the existing row; different bytes for the same (catalog, version) are a
-- conflict the client must resolve with a new version.
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_release_catalog_version
    ON skill_release (catalog_id, version);
-- One immutable archive is one release; identical bytes re-published under a
-- new version still name the same content.
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_release_bundle_sha256
    ON skill_release (bundle_sha256);
-- Idempotency-Key is opt-in; empty/NULL keys stay unconstrained so publishes
-- without a key do not collide with each other.
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_release_idempotency_key
    ON skill_release (idempotency_key)
    WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';
CREATE INDEX IF NOT EXISTS idx_skill_release_tenant_created
    ON skill_release (tenant_id, created_at DESC);

COMMENT ON TABLE skill_release IS 'Immutable server-attested publication records for tenant skill catalog archives (EXT-001)';
COMMENT ON COLUMN skill_release.archive_ref IS 'Opaque stored-object reference; never exposed to clients as a path';
COMMENT ON COLUMN skill_release.status IS 'published | delisted; delisting only stops new downloads';
