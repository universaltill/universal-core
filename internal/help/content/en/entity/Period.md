---
title: Period
audience: admin
module: finance
order: 3
---

A Period is one accounting period (typically a month) within a **Fiscal
Year** — the thing month-end close actually acts on. Unlike Fiscal Year's
own Open/Closed flag, a Period's status is the one the ledger genuinely
enforces: closing or locking a Period is what stops new postings dated
inside it.

## When to use it

Set these up for each Fiscal Year, covering the date ranges you want to
be able to close individually — most tenants create a Period per month.

## Creating a period

1. Go to **Period** and choose **New**.
2. Choose the **Fiscal Year** it belongs to.
3. Enter a Name (e.g. "2026-01"), Start Date, and End Date.
4. Status defaults to **Open**.
5. Save.

## Closing a period

Once you're confident nothing further should post against a period (the
usual month-end-close moment), set its Status to **Closed** or
**Locked** and save. From that point on, any journal entry — issuing a
Customer Invoice, receiving goods, posting due depreciation, or anything
else that reaches the ledger — dated inside that period's Start
Date/End Date range is rejected outright, with no override. Move the
posting's own date forward into an open period, or reopen this one, if
that turns out to be wrong.

## Rules to know

- Fiscal Year, Name, Start Date, and End Date are required.
- Status is **Open**, **Closed**, or **Locked** — both Closed and Locked
  block postings identically today; there is no current behavioral
  difference between them to rely on.
- A posting whose date falls inside more than one Period (nothing stops
  you from creating overlapping ones) is blocked if *any* covering
  Period is Closed or Locked, even if another one covering the same date
  is still Open.

## What it connects to

Every Period belongs to one **Fiscal Year**. The ledger checks every
Period's status — not the Fiscal Year's — before accepting any journal
entry's date, regardless of which module or action triggered that
posting.
