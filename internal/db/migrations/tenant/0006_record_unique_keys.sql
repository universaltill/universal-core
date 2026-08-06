-- record_unique_keys enforces entity.Definition.Unique's declarative
-- field-set constraints (uc-infra#81, ADR-0018 section 3 option c). One
-- row per (record, declared-constraint-name); the UNIQUE constraint below
-- is the actual correctness guarantee, not application code -- a Go-side
-- SELECT-before-INSERT check alone cannot close the race between two
-- concurrent transactions both inserting the same key (ADR-0018 section 3).
--
-- key_value is a JSON-encoded array of the constraint's field values, in
-- the field set's canonical (sorted) order -- computed in Go by
-- internal/kernel/crud's uniqueKeyValue, never by this migration.
--
-- record_id references records(id) so a hard delete of a record (none
-- exists today, but ON DELETE CASCADE costs nothing and avoids an orphan
-- if one is ever added) also clears its keys; an ordinary soft-delete
-- (records.deleted_at) does NOT cascade anything here -- crud.Engine.Delete
-- removes the row(s) itself, in the same transaction as the soft-delete,
-- so the key combination becomes reusable immediately.
CREATE TABLE record_unique_keys (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_type     TEXT NOT NULL,
    constraint_name TEXT NOT NULL,
    key_value       TEXT NOT NULL,
    record_id       UUID NOT NULL REFERENCES records(id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT record_unique_keys_key_uq UNIQUE (entity_type, constraint_name, key_value)
);

CREATE INDEX record_unique_keys_record_id_idx ON record_unique_keys (record_id);
