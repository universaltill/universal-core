-- uc-infra#205 / ADR-0004 addendum (2026-08-14): gl_accounts had no link
-- back to the finance.Account record it was projected from, so
-- finance.SyncGLAccountOnWrite/SyncGLAccounts could only upsert by
-- `code` (UpsertByCode) — renaming an Account's code (legal; code is
-- Unique but not immutable) made the sync path insert a NEW gl_accounts
-- row under the new code, leaving the old code's row behind, active,
-- unreachable from any Account. Pinned by
-- TestSyncGLAccountOnWrite_RenamingCodeOrphansThePreviousGLAccountsRow
-- (internal/kernel/finance/hooks_test.go) before this fix — renamed by
-- this same change to
-- TestSyncGLAccountOnWrite_RenamingCodeUpdatesTheSameRowInPlace, which
-- now asserts the fixed behavior instead.
--
-- source_record_id is the missing link: nullable (mirrors
-- 0008_workflow_jobs_input_hash.sql's own precedent for a column absent
-- on rows that predate it), UNIQUE so GLAccountRepo.UpsertBySourceRecord
-- (internal/data/ledger.go) can target it with ON CONFLICT and update
-- the existing row's code/name/account_type/currency/is_active in place
-- instead of inserting a second row.
--
-- GLAccountRepo.UpsertByCode (keyed by `code` alone) is UNCHANGED and
-- stays in place deliberately — ~15 test files across internal/data,
-- internal/api, internal/e2e and internal/kernel/{ledger,sales,
-- purchasing} call it directly to seed gl_accounts for ledger-posting
-- tests unrelated to this fix; only the two real finance.Account sync
-- paths (SyncGLAccountOnWrite, SyncGLAccounts) move to
-- UpsertBySourceRecord.
-- Deliberately NO foreign key to records(id): record_unique_keys does
-- have one (ON DELETE CASCADE), but that shape would be actively wrong
-- here. An ON DELETE CASCADE would delete a gl_accounts row the moment
-- its source records row is hard-deleted, even though journal_lines may
-- still hold an immutable FK to it — the exact corruption ADR-0004
-- exists to prevent. A plain (no-action) FK would also wire the
-- deterministic ledger core's own schema directly to the generic,
-- mutable records table, the coupling direction ADR-0004 keeps this
-- projection narrow specifically to avoid. source_record_id stays a
-- plain, unconstrained UUID; the only writer is GLAccountRepo itself.
ALTER TABLE gl_accounts ADD COLUMN source_record_id UUID;
ALTER TABLE gl_accounts ADD CONSTRAINT gl_accounts_source_record_id_key UNIQUE (source_record_id);

-- Backfill for gl_accounts rows created before this column existed:
-- correlate by `code` against the still-live Account record it came
-- from — but ONLY where exactly one live Account matches. Account.code
-- is enforced Unique at the entity layer going forward (uc-infra#204),
-- but that enforcement was never retroactively backfilled onto records
-- that predate it (crud/unique_constraints.go's BackfillUniqueConstraintKeys
-- deliberately skips, rather than repairs, a pre-existing duplicate) —
-- so two live Account records sharing the same code is a real state
-- this migration can encounter, not a hypothetical one. Linking to an
-- arbitrary pick between them would silently mis-attribute the
-- gl_accounts row to the wrong Account; leaving BOTH candidates
-- unlinked (same as a genuinely dead code with no match at all) is the
-- safe default — this only ever sets a previously-NULL column, never
-- touches an already-linked row or rewrites any other column, so
-- leaving an ambiguous case unlinked costs nothing but requires this
-- one card to fix its own duplicate codes before the affected row(s)
-- link automatically on a future migration re-run. A gl_accounts row
-- whose code matches no live Account at all (already orphaned before
-- this migration, e.g. from a rename that happened pre-fix) is left
-- unlinked the same way — nothing to correlate it to; out of scope for
-- this fix, same as the ADR-0004 addendum's own "not retroactive"
-- framing.
UPDATE gl_accounts ga
SET source_record_id = m.id
FROM (
    SELECT data ->> 'code' AS code, min(id::text)::uuid AS id
    FROM records
    WHERE entity_type = 'Account'
      AND deleted_at IS NULL
      AND data ->> 'code' IS NOT NULL
    GROUP BY 1
    HAVING count(*) = 1
) m
WHERE m.code = ga.code
  AND ga.source_record_id IS NULL;
