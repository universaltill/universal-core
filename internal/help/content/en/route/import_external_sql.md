---
title: Import from SQL Source
audience: user
module: import
order: 2
---

This is the second way to bring records in — instead of a CSV/XLSX file,
it pulls rows directly from a registered external database (a legacy
system's own database, reached over the network) and walks you through
the same mapping and preview steps as a file import.

## When to use it

Use it when the source data lives in a live database you can connect to
— a legacy ERP, an old accounting system — rather than something you'd
first export to a file. An administrator has to register that database
as an **External SQL Source** first (Settings → SQL Sources); this page
only reads from sources that are already registered.

## Importing from a source end to end

1. Open **Import from SQL source** from the regular Import page (or go
   directly to `/import/{EntityType}/sql`). If no source is registered
   yet, this page says so and links to Settings → SQL Sources instead of
   showing a browser.
2. Pick a registered source and select **Browse tables** to see its
   tables and views.
3. Select the table or view you want to import from. If it matches a
   known vendor template (for example, an NAV 2009 item or customer
   table), the column mapping is pre-filled from that template and
   flagged as such — review it the same as any suggested mapping.
   Otherwise the mapping is guessed from column names, same as the file
   import.
4. Optionally choose a **Key column**. With one selected, re-running the
   same import later *updates* the records it created before instead of
   creating duplicates; without one, every run creates new records, even
   if you run it again over the same source data.
5. Review the mapping, then **Preview** to see the rows that would be
   imported (validated the same way a file import's rows are), and
   **Commit** to actually write them.

## Rules to know

- **One run imports at most 10,000 rows.** The previewed rows are
  fetched again at commit time, so the source data should not change
  between preview and commit.
- Update-on-re-import (the Key column) needs a tenant-wide identity
  feature to be published; if it isn't, this page says so and import
  still works, just always as new records.
- The same hidden-field protections apply as for a file import: a field
  you don't have permission to see is never offered as a mapping target.
- A connection problem talking to the external database is reported
  generically here — the technical detail is only in the server log —
  same as the settings page's own **Test Connection** action.

## What it connects to

The source itself is configured on the **External SQL Source** settings
page (admin-only) before this page has anything to browse. Everything
downstream of the fetch — mapping, preview, commit, hidden-field
protection — is the same machinery the plain CSV/XLSX **Import** page
uses; an external table and an uploaded file are treated identically
once their rows are read.
