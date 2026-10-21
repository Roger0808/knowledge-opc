-- Migration: 000131_mcp_invocation_decisions
-- EXT-002 request-level approval decisions. One invocation has at most one
-- decision; the row binds the approver, the argument digest and the policy
-- version that were in force, so a later parameter or policy change cannot
-- silently ride an old approval.
CREATE TABLE IF NOT EXISTS mcp_invocation_decisions (
    invocation_id VARCHAR(36) PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    decided_by VARCHAR(512) NOT NULL DEFAULT '',
    -- decision is approve or deny.
    decision VARCHAR(16) NOT NULL,
    args_digest VARCHAR(64) NOT NULL,
    policy_version VARCHAR(64) NOT NULL DEFAULT '',
    decided_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_mcp_invocation_decisions_tenant
    ON mcp_invocation_decisions (tenant_id, decided_at DESC);

COMMENT ON TABLE mcp_invocation_decisions IS 'Single-effective request-level decisions for waiting MCP invocations (EXT-002)';
