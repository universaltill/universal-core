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
them — see Customer Invoice's own topic. In this release, a brand-new
Account isn't usable by the ledger the moment you save it — see "What it
connects to" below for why.

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
- The **Active** checkbox is a label on this record — in this release it
  does not, by itself, stop the ledger from accepting postings against
  the account. Whether the ledger actually rejects a deactivated
  account's code depends on whether an administrator has re-synchronized
  the ledger's own copy of the chart of accounts since you unchecked it
  (see "What it connects to" below); until that happens, postings keep
  going through exactly as before.
- Code is not schema-enforced unique — treat your own numbering as a
  convention to keep straight, the system won't catch a duplicate for
  you.

## What it connects to

A **Journal Entry**'s lines each reference one Account by its code —
issuing a Customer Invoice, receiving goods against a Purchase Order, and
posting due depreciation all post through Account codes your chart of
accounts must actually have. A **Period**'s open/closed/locked status —
not this record — is what the ledger checks before accepting a posting's
date; see Period's own topic. Account and Tax Code both feed the SAF-T
statutory export.

**A real limitation to know about in this release**: the ledger and the
SAF-T export don't read Account records directly — they read a separate,
internal copy of your chart of accounts that only an administrator
running a synchronization step (outside this screen, not something you
can trigger yourself here) keeps up to date. Adding, renaming, changing
the type of, or deactivating an Account has no effect on what the ledger
will accept, or on what a SAF-T export shows, until that synchronization
next runs. If a posting or an export doesn't reflect a change you just
made here, this is why — ask your administrator to re-synchronize.
