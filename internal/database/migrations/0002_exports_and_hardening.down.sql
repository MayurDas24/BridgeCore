-- 0002_exports_and_hardening.down.sql

DROP TRIGGER IF EXISTS trg_audit_logs_immutable ON audit_logs;
DROP FUNCTION IF EXISTS audit_logs_immutable();

DROP INDEX IF EXISTS idx_refresh_tokens_user_live;
DROP INDEX IF EXISTS idx_api_keys_prefix_live;
DROP INDEX IF EXISTS idx_api_keys_tenant_created;
DROP INDEX IF EXISTS idx_users_tenant_created;
DROP INDEX IF EXISTS idx_audit_logs_tenant_event_created;
DROP INDEX IF EXISTS idx_audit_logs_tenant_created;
DROP INDEX IF EXISTS idx_usage_logs_tenant_endpoint_method;
DROP INDEX IF EXISTS idx_usage_logs_tenant_created;

DROP TABLE IF EXISTS export_jobs;
