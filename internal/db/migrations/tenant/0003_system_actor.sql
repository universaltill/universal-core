-- A third actor kind: `system`.
--
-- ADR-0001 §14 makes actor identity first-class and distinguishes `human`
-- from `ai_agent` because an AI action carries model_version/input_hash a
-- human action does not. Scheduled workflow runs (R18) are honestly
-- neither: nobody clicked anything, and no model produced anything.
--
-- The alternative was reusing one of the existing two, and both lie in a
-- way that matters precisely where the audit trail is supposed to be
-- trustworthy: `human` invents a person who did not act, and `ai_agent`
-- claims a model was involved. An audit row that misattributes an
-- automated 3am run is worse than one extra enum value.
--
-- Append-only migration: the CHECK is replaced, not edited in place.
ALTER TABLE audit_log DROP CONSTRAINT audit_log_actor_type_check;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_actor_type_check
    CHECK (actor_type IN ('human', 'ai_agent', 'system'));

ALTER TABLE workflow_jobs DROP CONSTRAINT workflow_jobs_actor_type_check;
ALTER TABLE workflow_jobs ADD CONSTRAINT workflow_jobs_actor_type_check
    CHECK (actor_type IN ('human', 'ai_agent', 'system'));

-- A scheduled run (R18) is not about one record: nobody created or
-- updated anything, the clock simply came round. record_id/entity_type
-- were NOT NULL because every job until now was triggered by a record
-- event.
--
-- Made nullable rather than filled with a placeholder id: a synthetic
-- UUID would look like a real record to every reader and every join, and
-- "this job has no record" is exactly what NULL means.
ALTER TABLE workflow_jobs ALTER COLUMN record_id DROP NOT NULL;
ALTER TABLE workflow_jobs ALTER COLUMN entity_type DROP NOT NULL;
