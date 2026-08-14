---
title: Account
audience: admin
module: finance
order: 1
---

An Account is one line in your chart of accounts — the list of buckets
your business's money and obligations get sorted into (Cash, Accounts
Receivable, Sales Revenue, and so on). Every journal entry the system
posts debits and credits Accounts; nothing posts anywhere else.

## When to use it

Most tenants set up their chart of accounts once, early on, and touch it
rarely afterward — mainly to add a new account or deactivate one no
longer used. Accounts you don't set up yourself may already be expected
by the system: **Sales Revenue (code 4100)** and **Accounts Receivable
(code 1200)** are the codes issuing a Customer Invoice posts to, and if
your chart doesn't use those exact codes, that posting fails to find
them — see Customer Invoice's own topic. A brand-new Account is usable by
the ledger the moment you save it — no separate synchronization step
needed.

## Creating an account

1. Go to **Account** and choose **New**.
2. Enter its **Code** (your chart-of-accounts numbering, e.g. "1000") and
   **Name**.
3. Choose its **Type**: Asset, Liability, Equity, Income, or Expense.
4. Optionally set a **Parent Account** to nest it under another account
   (e.g. "1200 Accounts Receivable" under "1000 Assets"), and a
   **Currency** if this account should be tracked in a specific one.
5. Leave **Active** checked unless you're retiring the account.
6. Save.

## Rules to know

- Code and Name are required.
- Parent Account can be any other Account — nesting is unlimited, but the
  system does not check for a circular chain (an account set as its own
  ancestor), so avoid creating one by hand.
- The **Active** checkbox takes effect immediately — unchecking it stops
  the ledger from accepting new postings against the account as soon as
  you save.
- Code must be unique — the system rejects a second Account that reuses a
  code already in use by another one.

## What it connects to

A **Journal Entry**'s lines each reference one Account by its code —
issuing a Customer Invoice, receiving goods against a Purchase Order, and
posting due depreciation all post through Account codes your chart of
accounts must actually have. A **Period**'s open/closed/locked status —
not this record — is what the ledger checks before accepting a posting's
date; see Period's own topic. Account and Tax Code both feed the SAF-T
statutory export.

The ledger and the SAF-T export don't read Account records directly —
they read a separate, internal copy of your chart of accounts, kept
current automatically the moment you add, deactivate, or change the type
of an Account. No separate synchronization step is needed for any of
that.

**A narrower edge case to know about**: renaming an existing Account's
**Code** now relabels its internal ledger entry in place — the same
entry keeps tracking that account under the new code, and the old code
stops resolving to anything. There's no create-a-new-Account workaround
needed to correct a numbering mistake anymore; just rename it. The one
case a rename can be rejected: if the new code is still held by an old,
unlinked internal entry left over from before renames worked this way,
the save fails outright instead of silently reusing that entry — ask
your administrator if that happens.
