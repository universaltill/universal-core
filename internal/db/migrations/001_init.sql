-- Universal Core kernel: foundation schema.
-- Append-only after first release (ADR-0007 / CLAUDE.md).

CREATE TABLE tenants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    region      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Entity Definitions: the single source of truth for what an entity type
-- looks like (ADR-0017 §5). Versioned; draft/approve/publish/rollback.
CREATE TABLE entity_definitions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    entity_type TEXT NOT NULL,
    version     INT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'draft'
                CHECK (status IN ('draft', 'approved', 'published', 'rolled_back')),
    definition  JSONB NOT NULL,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('human', 'ai_agent')),
    created_by  TEXT NOT NULL,
    approved_by TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, entity_type, version)
);

CREATE INDEX idx_entity_definitions_current
    ON entity_definitions (tenant_id, entity_type, version DESC)
    WHERE status = 'published';

-- Form Definitions: layout for an entity type (ADR-0017 §6). Same
-- versioning discipline as entity_definitions.
CREATE TABLE form_definitions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    entity_type TEXT NOT NULL,
    version     INT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'draft'
                CHECK (status IN ('draft', 'approved', 'published', 'rolled_back')),
    definition  JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, entity_type, version)
);

-- Generic entity storage: every record of every entity type, foundation
-- and tenant-custom alike. Typed base-model columns can be promoted out
-- of `data` later (ADR-0017 §4/§7); `data` is the JSONB extension bag from
-- day one.
CREATE TABLE records (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    entity_type TEXT NOT NULL,
    data        JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);

CREATE INDEX idx_records_tenant_type ON records (tenant_id, entity_type)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_records_data_gin ON records USING GIN (data);

-- Audit log: every mutation, with AI-actor identity as a first-class
-- column from day one (ADR-0017 §14/§16), not retrofitted.
CREATE TABLE audit_log (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      UUID NOT NULL REFERENCES tenants(id),
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

CREATE INDEX idx_audit_log_tenant_record ON audit_log (tenant_id, entity_type, record_id);
CREATE INDEX idx_audit_log_actor ON audit_log (tenant_id, actor_type, actor_id);

-- Deterministic ledger core (ADR-0017 §1): hand-built, never AI-authored,
-- never touched by the generic entity engine directly.
CREATE TABLE gl_accounts (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    code        TEXT NOT NULL,
    name        TEXT NOT NULL,
    account_type TEXT NOT NULL CHECK (account_type IN
                 ('asset', 'liability', 'equity', 'income', 'expense')),
    currency    TEXT NOT NULL,
    is_active   BOOLEAN NOT NULL DEFAULT true,
    UNIQUE (tenant_id, code)
);

CREATE TABLE journal_entries (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants(id),
    entry_date   DATE NOT NULL,
    description  TEXT NOT NULL,
    source_type  TEXT,
    source_id    UUID,
    posted_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE journal_lines (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    journal_entry_id UUID NOT NULL REFERENCES journal_entries(id),
    account_id      UUID NOT NULL REFERENCES gl_accounts(id),
    debit_minor     BIGINT NOT NULL DEFAULT 0 CHECK (debit_minor >= 0),
    credit_minor    BIGINT NOT NULL DEFAULT 0 CHECK (credit_minor >= 0),
    CHECK (debit_minor = 0 OR credit_minor = 0)
);

CREATE INDEX idx_journal_lines_entry ON journal_lines (journal_entry_id);
CREATE INDEX idx_journal_lines_account ON journal_lines (account_id);
