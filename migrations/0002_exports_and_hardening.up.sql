-- 0002_exports_and_hardening.up.sql
--
-- Adds the asynchronous usage-export pipeline, the composite indexes that
-- every tenant-scoped read path actually needs, and database-level
-- immutability for the audit trail.

-- ============================================================
-- EXPORT JOBS  (asynchronous usage exports)
-- ============================================================
CREATE TABLE IF NOT EXISTS export_jobs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    requested_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    status        VARCHAR(20) NOT NULL DEFAULT 'queued'
                      CHECK (status IN ('queued', 'processing', 'completed', 'failed')),

    -- Filters are captured at request time so a job replayed by the worker
    -- produces byte-identical output to what the caller asked for.
    endpoint      VARCHAR(255) NOT NULL DEFAULT '',
    method        VARCHAR(10)  NOT NULL DEFAULT '',
    from_ts       TIMESTAMPTZ,
    to_ts         TIMESTAMPTZ,

    object_key    VARCHAR(512) NOT NULL DEFAULT '',
    row_count     INTEGER      NOT NULL DEFAULT 0,
    size_bytes    BIGINT       NOT NULL DEFAULT 0,
    attempts      INTEGER      NOT NULL DEFAULT 0,
    error         TEXT         NOT NULL DEFAULT '',
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Tenant-scoped listing: the only read pattern the API exposes.
CREATE INDEX IF NOT EXISTS idx_export_jobs_tenant_created
    ON export_jobs (tenant_id, created_at DESC);

-- The worker's claim query (`status = 'queued' ORDER BY created_at
-- FOR UPDATE SKIP LOCKED`) touches only queued rows, so the index is
-- partial: completed jobs accumulate forever and must not bloat it.
CREATE INDEX IF NOT EXISTS idx_export_jobs_claimable
    ON export_jobs (created_at)
    WHERE status = 'queued';

-- Lets an operator find stuck jobs (processing but never finished).
CREATE INDEX IF NOT EXISTS idx_export_jobs_processing
    ON export_jobs (started_at)
    WHERE status = 'processing';

-- ============================================================
-- COMPOSITE INDEXES FOR TENANT-SCOPED READ PATHS
-- ============================================================
-- Every list endpoint filters by tenant_id and orders by created_at DESC.
-- A single-column index on tenant_id alone still forces a sort of every
-- matching row, which is exactly the query that degrades first as a busy
-- tenant's usage table grows.
CREATE INDEX IF NOT EXISTS idx_usage_logs_tenant_created
    ON usage_logs (tenant_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_usage_logs_tenant_endpoint_method
    ON usage_logs (tenant_id, endpoint, method);

CREATE INDEX IF NOT EXISTS idx_audit_logs_tenant_created
    ON audit_logs (tenant_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_audit_logs_tenant_event_created
    ON audit_logs (tenant_id, event, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_users_tenant_created
    ON users (tenant_id, created_at DESC)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_api_keys_tenant_created
    ON api_keys (tenant_id, created_at DESC);

-- API-key authentication looks up every live key sharing a prefix on each
-- request, so that lookup gets its own partial index.
CREATE INDEX IF NOT EXISTS idx_api_keys_prefix_live
    ON api_keys (prefix)
    WHERE is_active = TRUE AND revoked_at IS NULL;

-- Refresh-token verification lists a user's live tokens on every refresh.
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user_live
    ON refresh_tokens (user_id, expires_at)
    WHERE revoked_at IS NULL;

-- ============================================================
-- AUDIT IMMUTABILITY
-- ============================================================
-- An audit trail that the application can rewrite is not an audit trail.
-- The API only ever INSERTs and SELECTs; this trigger makes that a
-- database-enforced guarantee rather than a convention, so a future bug
-- (or a compromised application credential) cannot quietly erase history.
-- Deliberate retention work runs as a migration by a privileged role, which
-- can drop the trigger first.
CREATE OR REPLACE FUNCTION audit_logs_immutable()
RETURNS TRIGGER AS $audit_immutable$
BEGIN
    RAISE EXCEPTION 'audit_logs is append-only: % is not permitted', TG_OP;
END;
$audit_immutable$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_audit_logs_immutable ON audit_logs;
CREATE TRIGGER trg_audit_logs_immutable
    BEFORE UPDATE OR DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION audit_logs_immutable();
