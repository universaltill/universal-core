-- Backs assets.PostDueDepreciationBatch's two bounded queries
-- (universaltill/uc-infra#182 — replacing the previous shape, which
-- read and decoded every DepreciationSchedule row in the tenant on
-- every call):
--
--  1. RecordRepo.ListDueUnposted's posting worklist: WHERE
--     entity_type = 'DepreciationSchedule' AND deleted_at IS NULL AND
--     coalesce(data->>'posted_at','') = '' AND data->>'period_end' <>
--     '' AND data->>'period_end' <= $today ORDER BY created_at, id
--     LIMIT $maxRows.
--  2. RecordRepo.LifeCompleteGroupIDs' completion/healing sweep's
--     correlated NOT EXISTS / count(*) subqueries, both scoped to one
--     FixedAsset's own rows via data->>'fixed_asset_id'.
--
-- Scoped to this one entity type and these three fields, all known at
-- migration-authoring time -- NOT the generic "arbitrary
-- Definition-declared sortable/filterable field" problem #95 tracks;
-- see 0005_records_list_sort_index.sql's own comment for that
-- distinction.
CREATE INDEX idx_records_depreciation_schedule_due
    ON records ((data->>'period_end'))
    WHERE entity_type = 'DepreciationSchedule'
      AND deleted_at IS NULL
      AND coalesce(data->>'posted_at', '') = '';

CREATE INDEX idx_records_depreciation_schedule_asset
    ON records ((data->>'fixed_asset_id'))
    WHERE entity_type = 'DepreciationSchedule'
      AND deleted_at IS NULL;

-- Reaching EXISTING tenant databases needs an explicit operator step
-- (`cmd/migrate -target tenant` against each one) -- internal/tenantdb.
-- Router only auto-applies the tenant migration set when provisioning a
-- brand-new tenant, not on every open of an existing one (same caveat
-- 0005_records_list_sort_index.sql's own header already documents).
