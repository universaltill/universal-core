---
title: Import Records
audience: user
module: import
order: 1
---

Import lets you bulk-create records for one entity type from a CSV or
XLSX file, with a mapping step you confirm before anything is written.

## When to use it

Use it whenever you have many records to create at once — a vendor list,
a starting item catalog, a batch of contacts — rather than typing them
in one at a time.

## Importing a file end to end

1. Open **Import** from the entity's own list page (next to **New** and
   **Export**), or go directly to `/import/{EntityType}`.
2. Choose a `.csv` or `.xlsx` file (up to 50 MiB) and select **Preview**.
3. The system suggests a mapping from your file's columns to the
   entity's fields, matching column names to field names automatically.
   If an AI provider is configured for this tenant, columns it could not
   match by name are given an AI-suggested guess instead, marked
   **(AI-suggested — please confirm)** — always double-check those before
   continuing.
4. Review and adjust the mapping using the dropdown next to each column,
   then select **Preview** again. Every row is validated against the
   entity's own rules and shown with a status of **OK** or **Error**
   before anything is committed.
5. Once the mapping is complete and you're satisfied with the preview,
   select **Commit**. Rows that pass validation are created; rows that
   fail are not, and are never partially written.
6. The result screen reports how many rows succeeded, how many failed,
   and the reason for each failure.

## Rules to know

- **Nothing is written during Preview** — only Commit actually creates
  records. Re-previewing after adjusting the mapping is always safe.
- **A row that fails validation does not block the others.** Commit is
  row-by-row: some rows can succeed while others fail in the same run.
- **A field you don't have permission to see cannot be a mapping
  target.** The mapping dropdowns never offer a hidden field, and
  mapping to one by hand (or leaving a hidden Required field unmapped)
  is refused with a generic message rather than naming the field.
- Committing re-submits the same file you originally chose — the file
  input on this page is never cleared by the Preview step, so leave it
  as-is between Preview and Commit.

## What it connects to

Import targets one entity type at a time, using whatever CRUD rules and
field permissions already apply to records created any other way — an
imported row is validated and written exactly like one entered by hand.
If your data instead lives in another live database rather than a file,
see **Import from SQL Source** for the alternative, reached from this
same page.
