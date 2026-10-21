-- Migration: 000130_mcp_invocations
-- EXT-002 per-tool precise MCP invocation records. One row is one request-level
-- MCP tool call: the caller creates it, the policy check drives it through
-- waiting_approval / waiting_oauth / executing, and the terminal status keeps
-- the actual upstream receipt (succeeded / failed / unknown_outcome). A call
-- is never auto-retried after an unknown outcome; cancel records the real
-- receipt instead of claiming a rollback.
CREATE TABLE IF NOT EXISTS mcp_invocations (
    invocation_id VARCHAR(36) PRIMARY KEY DEFAULT uuid_generate_v4(),
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
    -- no encryption key is configured.
    args_encrypted BYTEA,
    -- idempotency_key replays return the original invocation row. Empty keys
    -- stay unconstrained so keyless calls do not collide with each other.
    idempotency_key TEXT,
    -- status: created | policy_checked | waiting_approval | waiting_oauth |
    -- executing | succeeded | failed | cancelled | cancel_requested |
    -- unknown_outcome
    status VARCHAR(32) NOT NULL DEFAULT 'created',
    -- policy_version stamps which stored policy row authorized the call.
    policy_version VARCHAR(64) NOT NULL DEFAULT '',
    -- context_jti is the one-time id of the platform-signed invocation
    -- context; the partial unique index makes replay a constraint error.
    context_jti VARCHAR(64) NOT NULL DEFAULT '',
    -- result_ref carries the upstream receipt (JSON) for terminal states.
    result_ref JSONB,
    -- error carries the terminal failure reason, e.g. policy_denied, denied,
    -- expired, oauth_timeout, upstream_error.
    error TEXT,
    sent_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    -- expires_at defaults to 7 days; afterwards the result is cleared and
    -- only the status remains queryable.
    expires_at TIMESTAMPTZ NOT NULL DEFAULT (CURRENT_TIMESTAMP + INTERVAL '7 days'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
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

COMMENT ON TABLE mcp_invocations IS 'Request-level MCP tool invocations with policy, approval and receipt lifecycle (EXT-002)';
COMMENT ON COLUMN mcp_invocations.args_digest IS 'SHA-256 of canonical invocation arguments; decisions bind to this digest';
COMMENT ON COLUMN mcp_invocations.status IS 'created|policy_checked|waiting_approval|waiting_oauth|executing|succeeded|failed|cancelled|cancel_requested|unknown_outcome';
