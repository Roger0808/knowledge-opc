-- Migration: 000046_opc_integration (SQLite)
-- SQLite equivalents of the versioned (PostgreSQL) migrations
-- 000129_skill_release, 000130_mcp_invocations and
-- 000131_mcp_invocation_decisions.
--
-- skill_release: immutable, server-side attested publication records of
-- tenant_skill_catalog archives (EXT-001). At publish time the archive bytes
-- are re-hashed server-side and copied under the immutable per-release key
-- tenant-skills/releases/<release_id>.zip that archive_ref names, so a later
-- catalog re-register or delete never changes what a release serves.
CREATE TABLE IF NOT EXISTS skill_release (
    id VARCHAR(36) PRIMARY KEY,
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

-- mcp_invocations: request-level MCP tool invocations with policy, approval
-- and receipt lifecycle (EXT-002). A call is never auto-retried after an
-- unknown outcome; cancel records the real receipt instead of claiming a
-- rollback.
CREATE TABLE IF NOT EXISTS mcp_invocations (
    invocation_id VARCHAR(36) PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    -- subject is the authenticated caller (user id or task principal).
    subject VARCHAR(512) NOT NULL DEFAULT '',
    -- actor_kind is user or task; task actors hold no decision rights.
    actor_kind VARCHAR(16) NOT NULL DEFAULT 'user',
    -- task_id / device bind an unattended (task) context to a concrete run.
    task_id VARCHAR(64),
    device VARCHAR(128),
    service_id VARCHAR(36) NOT NULL,
    -- tool_name is the raw tool name on the MCP service (never prefixed).
    tool_name VARCHAR(512) NOT NULL,
    -- args_digest is the SHA-256 of the canonical invocation arguments; the
    -- decision binds to it, so a parameter change invalidates the decision.
    args_digest VARCHAR(64) NOT NULL,
    -- args_encrypted holds the AES-256-GCM sealed argument body; NULL when
    -- no encryption key is configured (arguments are never stored in
    -- plaintext).
    args_encrypted BLOB,
    -- idempotency_key replays return the original invocation row. Empty keys
    -- stay unconstrained so keyless calls do not collide with each other.
    idempotency_key TEXT,
    -- status: created | policy_checked | waiting_approval | waiting_oauth |
    -- executing | succeeded | failed | cancelled | cancel_requested |
    -- unknown_outcome
    status VARCHAR(32) NOT NULL DEFAULT 'created',
    -- policy_version stamps which stored policy row authorized the call.
    policy_version VARCHAR(64) NOT NULL DEFAULT '',
    -- risk records the tool risk class at policy check (R2): the crash sweep
    -- settles mutating calls past WriteTimeout and reads past the longer
    -- ReadSweepTimeout; empty means legacy rows, swept with the write window.
    risk VARCHAR(16) NOT NULL DEFAULT '',
    -- context_jti is the one-time id of the platform-signed invocation
    -- context; the partial unique index makes replay a constraint error.
    context_jti VARCHAR(64) NOT NULL DEFAULT '',
    -- result_ref carries the upstream receipt (JSON) for terminal states.
    result_ref TEXT,
    -- error carries the terminal failure reason, e.g. policy_denied, denied,
    -- expired, oauth_timeout, upstream_error.
    error TEXT,
    sent_at DATETIME,
    finished_at DATETIME,
    expires_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_mcp_invocations_tenant_created
    ON mcp_invocations (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_mcp_invocations_subject
    ON mcp_invocations (tenant_id, subject);
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_invocations_idempotency_key
    ON mcp_invocations (idempotency_key)
    WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_invocations_context_jti
    ON mcp_invocations (context_jti)
    WHERE context_jti IS NOT NULL AND context_jti <> '';

-- mcp_invocation_decisions: single-effective request-level decisions for
-- waiting MCP invocations. The row binds the approver, the argument digest
-- and the policy version that were in force, so a later parameter or policy
-- change cannot silently ride an old approval.
CREATE TABLE IF NOT EXISTS mcp_invocation_decisions (
    invocation_id VARCHAR(36) PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    decided_by VARCHAR(512) NOT NULL DEFAULT '',
    -- decision is approve or deny.
    decision VARCHAR(16) NOT NULL,
    args_digest VARCHAR(64) NOT NULL,
    policy_version VARCHAR(64) NOT NULL DEFAULT '',
    decided_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_mcp_invocation_decisions_tenant
    ON mcp_invocation_decisions (tenant_id, decided_at DESC);
