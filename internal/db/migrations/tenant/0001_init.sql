-- Per-tenant database schema (ADR-0003): everything that used to be
-- tenant_id-scoped in one shared database now lives here instead, one
-- database per tenant, tenant_id columns removed entirely — a query
-- against this database can only ever see this one tenant's rows,
-- physically, not by convention.
--
-- This is a brand-new kind of database with no prior migration history,
-- so this baseline is the target shape directly (equivalent to the
-- original 001_init.sql + 003_definition_registry.sql's
-- form_definitions/workflow_definitions additions +
-- 005_record_version.sql's records.version column, merged into one
-- baseline), not a replayed patch sequence. The original migrations
-- (internal/db/migrations/*.sql) are left untouched — they're what
-- actually ran against the pre-migration shared database, protected by
-- CLAUDE.md's append-only rule, not a template to keep editing.

CREATE TABLE entity_definitions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_type     TEXT NOT NULL,
    version         INT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'draft'
                    CHECK (status IN ('draft', 'approved', 'published', 'rolled_back')),
    definition      JSONB NOT NULL,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('human', 'ai_agent')),
    created_by      TEXT NOT NULL,
    approved_by     TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (entity_type, version)
);

CREATE INDEX idx_entity_definitions_current
    ON entity_definitions (entity_type, version DESC)
    WHERE status = 'published';

CREATE TABLE form_definitions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_type     TEXT NOT NULL,
    version         INT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'draft'
                    CHECK (status IN ('draft', 'approved', 'published', 'rolled_back')),
    definition      JSONB NOT NULL,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('human', 'ai_agent')),
    created_by      TEXT NOT NULL,
    approved_by     TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (entity_type, version)
);

CREATE INDEX idx_form_definitions_current
    ON form_definitions (entity_type, version DESC)
    WHERE status = 'published';

-- Keyed by (name, version) rather than entity_type — workflow.Definition's
-- own identity is Name, not an entity type.
CREATE TABLE workflow_definitions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    version         INT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'draft'
                    CHECK (status IN ('draft', 'approved', 'published', 'rolled_back')),
    definition      JSONB NOT NULL,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('human', 'ai_agent')),
    created_by      TEXT NOT NULL,
    approved_by     TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (name, version)
);

CREATE INDEX idx_workflow_definitions_current
    ON workflow_definitions (name, version DESC)
    WHERE status = 'published';

-- Generic entity storage: every record of every entity type, foundation
-- and tenant-custom alike. version is the optimistic-locking counter
-- (starts at 1 — see the original 005_record_version.sql's doc comment
-- for why not 0).
CREATE TABLE records (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_type TEXT NOT NULL,
    data        JSONB NOT NULL DEFAULT '{}',
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);

CREATE INDEX idx_records_type ON records (entity_type)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_records_data_gin ON records USING GIN (data);

-- Audit log: every mutation, with AI-actor identity as a first-class
-- column from day one (ADR-0001 §14/§16).
CREATE TABLE audit_log (
    id             BIGSERIAL PRIMARY KEY,
    entity_type    TEXT NOT NULL,
    record_id      UUID,
    action         TEXT NOT NULL CHECK (action IN ('create', 'update', 'delete')),
    actor_type     TEXT NOT NULL CHECK (actor_type IN ('human', 'ai_agent')),
    actor_id       TEXT NOT NULL,
    model_version  TEXT,
    input_hash     TEXT,
    diff           JSONB,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_log_record ON audit_log (entity_type, record_id);
CREATE INDEX idx_audit_log_actor ON audit_log (actor_type, actor_id);

-- Deterministic ledger core (ADR-0001 §1): hand-built, never AI-authored,
-- never touched by the generic entity engine directly.
CREATE TABLE gl_accounts (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code         TEXT NOT NULL,
    name         TEXT NOT NULL,
    account_type TEXT NOT NULL CHECK (account_type IN
                 ('asset', 'liability', 'equity', 'income', 'expense')),
    currency     TEXT NOT NULL,
    is_active    BOOLEAN NOT NULL DEFAULT true,
    UNIQUE (code)
);

CREATE TABLE journal_entries (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entry_date  DATE NOT NULL,
    description TEXT NOT NULL,
    source_type TEXT,
    source_id   UUID,
    posted_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE journal_lines (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    journal_entry_id UUID NOT NULL REFERENCES journal_entries(id),
    account_id       UUID NOT NULL REFERENCES gl_accounts(id),
    debit_minor      BIGINT NOT NULL DEFAULT 0 CHECK (debit_minor >= 0),
    credit_minor     BIGINT NOT NULL DEFAULT 0 CHECK (credit_minor >= 0),
    CHECK (debit_minor = 0 OR credit_minor = 0)
);

CREATE INDEX idx_journal_lines_entry ON journal_lines (journal_entry_id);
CREATE INDEX idx_journal_lines_account ON journal_lines (account_id);

-- Durable Postgres job queue for workflow execution (ADR-0001 §9).
-- ClaimNext/ReclaimStale (internal/data's WorkflowJobRepo) are
-- deliberately tenant-global in the old shared-DB design; per-tenant
-- databases mean the worker fans out across every tenant's database
-- instead of running one cross-tenant query (internal/worker's
-- Runner.tick) — this table itself is otherwise unchanged, just
-- tenant_id-free like everything else here.
CREATE TABLE workflow_jobs (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_name    TEXT NOT NULL,
    workflow_version INT NOT NULL,
    entity_type      TEXT NOT NULL,
    record_id        UUID NOT NULL,
    step_index       INT NOT NULL DEFAULT 0,
    status           TEXT NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued', 'running', 'waiting_approval', 'done', 'dead_letter')),
    attempts         INT NOT NULL DEFAULT 0,
    max_attempts     INT NOT NULL DEFAULT 5,
    last_error       TEXT,
    run_after        TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_type       TEXT NOT NULL CHECK (actor_type IN ('human', 'ai_agent')),
    actor_id         TEXT NOT NULL,
    model_version    TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_workflow_jobs_claimable
    ON workflow_jobs (run_after)
    WHERE status = 'queued';

CREATE INDEX idx_workflow_jobs_status
    ON workflow_jobs (status);
