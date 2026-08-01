-- Backs the DEFAULT list-page query path (RecordRepo.ListPage /
-- ListPageFiltered when no SortField is given, i.e. every list page
-- before a user picks a column to sort by): WHERE entity_type = $1 AND
-- deleted_at IS NULL ORDER BY created_at ..., id LIMIT/OFFSET.
--
-- Measured (universaltill/uc-infra#50): idx_records_type alone (just
-- entity_type) lets Postgres narrow to the right rows but still reads
-- and sorts every one of them in memory on every page, for every
-- entity type, on every request — the exact "seq scan + in-memory
-- sort" the issue flagged, just for the unsorted/default path rather
-- than an explicit custom-field sort.
--
-- A per-field JSONB expression/trigram index for arbitrary,
-- user-chosen sort/filter fields (data->>'field') is a separate,
-- materially bigger problem: fields are runtime Entity Definition
-- metadata, not known at migration-authoring time, so indexing them
-- generically needs dynamic DDL tied to publish-time metadata, not a
-- static migration — out of scope here, tracked as a new backlog card.
CREATE INDEX idx_records_type_created
    ON records (entity_type, created_at)
    WHERE deleted_at IS NULL;
