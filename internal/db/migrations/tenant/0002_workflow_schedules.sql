-- Scheduled workflows (R18). One row per scheduled workflow in this
-- tenant, carrying when it is next due.
--
-- Separate from workflow_jobs deliberately: a job is one execution, a
-- schedule is the standing intent to create them. Folding the two would
-- mean deriving "when is it next due" by scanning execution history,
-- which gets slower exactly as a tenant runs more jobs.
CREATE TABLE workflow_schedules (
    -- One schedule per workflow name. Versions supersede rather than
    -- accumulate: publishing v2 of a nightly job means the tenant wants
    -- one nightly job, not two.
    workflow_name    TEXT PRIMARY KEY,
    workflow_version INT NOT NULL,
    -- Kept alongside next_due_at so a changed expression is detectable
    -- without re-reading the definition registry on every tick.
    cron             TEXT NOT NULL,
    next_due_at      TIMESTAMPTZ NOT NULL,
    last_fired_at    TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The only query the worker runs on every tick, for every tenant.
CREATE INDEX idx_workflow_schedules_due ON workflow_schedules (next_due_at);
