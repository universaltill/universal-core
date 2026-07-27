-- Control-plane database schema (ADR-0003): the tenant registry only.
-- Everything that used to be tenant_id-scoped in the original shared
-- database (internal/db/migrations/*.sql, left untouched — that schema
-- is what actually ran against the live pre-migration database, not
-- edited per CLAUDE.md's append-only rule) now lives in its own
-- per-tenant database instead; see internal/db/migrations/tenant/.
--
-- This is a brand-new database with no prior migration history of its
-- own, so this is a fresh baseline (equivalent to the original
-- 001_init.sql's tenants table plus 004_tenant_zitadel_org.sql's
-- zitadel_org_id column, merged into one CREATE TABLE), not a patch
-- applied on top of anything.
CREATE TABLE tenants (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT NOT NULL,
    region         TEXT NOT NULL,
    -- The database that holds this tenant's own data (records,
    -- audit_log, workflow_jobs, definitions, ledger) — internal/tenantdb's
    -- Router resolves a tenant_id to a *sql.DB by looking this up, then
    -- connecting to the same Postgres server with this dbname instead of
    -- the control-plane one.
    db_name        TEXT NOT NULL UNIQUE,
    zitadel_org_id TEXT UNIQUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
