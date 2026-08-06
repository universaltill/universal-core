-- Backs JournalEntryRepo.ExistsForSource (internal/data/ledger.go):
-- SELECT EXISTS(SELECT 1 FROM journal_entries WHERE source_type = $1 AND
-- source_id = $2) -- called once per row by every posting hook that
-- guards against a double post (sales.PostCustomerInvoiceToLedger,
-- purchasing.PostGoodsReceiptLineToLedger, and, as of uc-infra#76,
-- assets.PostDueDepreciation -- the first of these to call it in a loop
-- over potentially many rows per run rather than once per user action,
-- which is what surfaced the missing index: independent review measured
-- this as a full sequential scan of journal_entries per call, with no
-- index on either column it filters by.
--
-- source_type/source_id are both nullable (an entry with neither is a
-- manual/non-sourced posting, if this kernel ever allows one) --
-- WHERE source_id IS NOT NULL keeps those rows out of the index
-- entirely, since ExistsForSource is never called with an empty
-- sourceID and a full-table index over rows this query can never match
-- would only add write overhead for no read benefit.
CREATE INDEX idx_journal_entries_source
    ON journal_entries (source_type, source_id)
    WHERE source_id IS NOT NULL;

-- Reaching EXISTING tenant databases needs an explicit operator step
-- (`cmd/migrate -target tenant` against each one) -- internal/tenantdb.
-- Router only auto-applies the tenant migration set when provisioning a
-- brand-new tenant, not on every open of an existing one (same caveat
-- 0005_records_list_sort_index.sql's own header already documents).
