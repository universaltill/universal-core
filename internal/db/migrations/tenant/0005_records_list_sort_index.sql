-- Backs the DEFAULT list-page query path (RecordRepo.ListPage /
-- ListPageFiltered when no SortField is given, i.e. before a user picks
-- a column to sort by AND no filter box narrows the result): WHERE
-- entity_type = $1 AND deleted_at IS NULL ORDER BY created_at ..., id
-- LIMIT/OFFSET. Measured: first-page cost on this exact path drops from
-- ~13ms to ~0.14ms at 15k rows/entity-type (universaltill/uc-infra#50).
-- Does NOT help a query that also carries a filter (data->>'field'
-- ILIKE ...) or a deep OFFSET page — those still read/sort every
-- matching row; see #50 and the split-out #95 for the harder, per-field
-- case this doesn't attempt.
--
-- A per-field JSONB expression/trigram index for arbitrary,
-- user-chosen sort/filter fields (data->>'field') is a separate,
-- materially bigger problem: fields are runtime Entity Definition
-- metadata, not known at migration-authoring time, so indexing them
-- generically needs dynamic DDL tied to publish-time metadata, not a
-- static migration — out of scope here, tracked as universaltill/uc-infra#95.
--
-- Reaching EXISTING tenant databases needs an explicit operator step
-- (`cmd/migrate -target tenant` against each one) — internal/tenantdb.
-- Router only auto-applies the tenant migration set when provisioning a
-- brand-new tenant, not on every open of an existing one.
CREATE INDEX idx_records_type_created
    ON records (entity_type, created_at)
    WHERE deleted_at IS NULL;
