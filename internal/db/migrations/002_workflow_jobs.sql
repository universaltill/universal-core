-- Durable Postgres job queue for workflow execution (ADR-0001 §9):
-- retries with backoff, dead-letter after max_attempts, and a resumable
-- step_index so a crashed worker's in-flight job is picked up by another
-- worker rather than lost. Append-only alongside 001_init.sql (ADR-0007 /
-- CLAUDE.md).

CREATE TABLE workflow_jobs (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL REFERENCES tenants(id),
    workflow_name    TEXT NOT NULL,
    workflow_version INT NOT NULL,
    entity_type      TEXT NOT NULL,
    record_id        UUID NOT NULL,
    step_index       INT NOT NULL DEFAULT 0,
    -- No plain 'failed' status: MarkFailed only ever transitions a job to
    -- 'queued' (attempts remain) or 'dead_letter' (exhausted) — a failure
    -- that stops retrying is dead_letter, not a separate terminal state.
    status           TEXT NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued', 'running', 'waiting_approval', 'done', 'dead_letter')),
    attempts         INT NOT NULL DEFAULT 0,
    max_attempts     INT NOT NULL DEFAULT 5,
    last_error       TEXT,
    run_after        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- AI-actor identity first-class on the job too (ADR-0001 §14/§16),
    -- same three columns as audit_log: who/what caused this workflow run.
    actor_type       TEXT NOT NULL CHECK (actor_type IN ('human', 'ai_agent')),
    actor_id         TEXT NOT NULL,
    model_version    TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Claim query: WHERE status = 'queued' AND run_after <= now() ORDER BY
-- run_after FOR UPDATE SKIP LOCKED LIMIT 1 — this partial index keeps that
-- scan cheap as the table fills with done/dead_letter history.
CREATE INDEX idx_workflow_jobs_claimable
    ON workflow_jobs (run_after)
    WHERE status = 'queued';

CREATE INDEX idx_workflow_jobs_tenant_status
    ON workflow_jobs (tenant_id, status);
