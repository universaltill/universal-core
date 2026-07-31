-- Adds 'export' to audit_log's action CHECK: a statutory audit-file
-- export (SAF-T, universaltill/uc-infra#28) is a bulk disclosure of the
-- tenant's ledger — not a mutation, but still an act ADR-0001 §14's
-- actor accountability must attribute. Same drop-and-recreate shape as
-- 0003_system_actor.sql's actor_type widening; append-only per the
-- migration discipline.
ALTER TABLE audit_log DROP CONSTRAINT audit_log_action_check;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_action_check
    CHECK (action IN ('create', 'update', 'delete', 'export'));
