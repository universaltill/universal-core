-- Completes the Definition registry ADR-0001 §5/§6/§14 call for: brings
-- form_definitions up to the same AI-actor/approval tracking
-- entity_definitions already had in 001_init.sql (an asymmetry with no
-- principled reason — both are AI-authorable per §14's own list of
-- "entity, field, mapping, workflow, form"), and adds workflow_definitions,
-- the one Definition kind 001_init.sql didn't create a table for yet
-- (internal/kernel/workflow.Queue.ProcessOne has taken an injected
-- DefinitionLookup callback as an explicit stopgap for this).

ALTER TABLE form_definitions
    ADD COLUMN created_by_type TEXT NOT NULL CHECK (created_by_type IN ('human', 'ai_agent')),
    ADD COLUMN created_by      TEXT NOT NULL,
    ADD COLUMN approved_by     TEXT;

-- Workflow Definitions: same versioned/draft-approve-publish-rollback
-- discipline as entity_definitions/form_definitions. Keyed by (tenant_id,
-- name, version) rather than entity_type — workflow.Definition's own
-- identity is Name, not an entity type (a workflow can trigger from an
-- entity event, but isn't itself scoped to one entity type the way a
-- form is).
CREATE TABLE workflow_definitions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    name        TEXT NOT NULL,
    version     INT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'draft'
                CHECK (status IN ('draft', 'approved', 'published', 'rolled_back')),
    definition  JSONB NOT NULL,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('human', 'ai_agent')),
    created_by  TEXT NOT NULL,
    approved_by TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name, version)
);

-- "Current" is the highest version among published rows — publishing a
-- new version never has to touch older published rows (they simply stop
-- being current once a higher version exists), and rolling back the
-- current version naturally falls back to the next-highest published
-- one. Same pattern as idx_entity_definitions_current/
-- idx_form_definitions_current below.
CREATE INDEX idx_workflow_definitions_current
    ON workflow_definitions (tenant_id, name, version DESC)
    WHERE status = 'published';

-- entity_definitions already had this index from 001_init.sql; add the
-- matching one for form_definitions, which 001_init.sql omitted.
CREATE INDEX idx_form_definitions_current
    ON form_definitions (tenant_id, entity_type, version DESC)
    WHERE status = 'published';
