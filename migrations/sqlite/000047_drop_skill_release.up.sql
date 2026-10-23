-- Migration: 000047_drop_skill_release (SQLite)
-- WORK-RESOURCES-001 / ADR-008 / DATA-001: SQLite equivalent of versioned
-- 000133. Only skill_release and the indexes owned by that table are
-- dropped. The MCP tables created by the same combined migration
-- 000046_opc_integration (mcp_invocations, mcp_invocation_decisions) stay —
-- 000046's down must NOT be used to undo this cleanup.

DROP INDEX IF EXISTS idx_skill_release_tenant_created;
DROP INDEX IF EXISTS idx_skill_release_idempotency_key;
DROP INDEX IF EXISTS idx_skill_release_bundle_sha256;
DROP INDEX IF EXISTS idx_skill_release_catalog_version;

DROP TABLE IF EXISTS skill_release;
