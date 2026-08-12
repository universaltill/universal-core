---
title: External Identity
audience: admin
module: foundation
order: 24
---

An External Identity remembers which record in an external legacy system a
record here came from — its source, the record type it landed as, and the
legacy system's own key for it. This is what lets a repeated import update
the same record it created last time, instead of creating a duplicate every
time you re-run it.

## When to use it

You never create or edit these yourself — they are written automatically by
the import engine whenever it commits an import run through the SQL or CSV
import wizard. There is no "New" screen for this record type.

## What it looks like in practice

Each row records the source it came from, the exact table or view it was
read from, the record type and record it produced here, and the legacy
system's own identifying key for it (for example, a customer number from
the legacy system). The next time you import from the same source and see
that same key again, the import updates the record it already created
instead of adding a second copy.

## Rules to know

- The combination of source, the table or view it came from, record type,
  and legacy key must be unique — the import engine relies on this to find
  the right record to update, and a duplicate combination is rejected with
  an error saying the combination is already in use.
- There is no data-entry form for this record type, by design — hand-
  editing an identity row would silently break the "update, don't
  duplicate" behavior a re-import depends on. It is written only by the
  import engine.
- Write access to External Identity through the ordinary record screens is
  denied to everyone, always — including a `tenant_admin`. Unlike most
  other control-plane record types, this one has no admin bypass at all;
  the import engine is the only trusted writer, which is also why there is
  no "New" screen (above) to even attempt it from.

## What it connects to

Every External Identity references an **External SQL Source** and the
record it produced (by that record's type and ID). It is the bookkeeping
behind a repeatable import, not something you interact with directly.
