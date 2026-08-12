---
title: Fiscal Year
audience: admin
module: finance
order: 2
---

A Fiscal Year is the top level of your accounting calendar — the span a
year's worth of **Period** records belong to. Most businesses use the
calendar year, but this doesn't assume that: set the Start Date and End
Date to whatever your business actually reports on.

## When to use it

Set up a Fiscal Year before adding the Periods inside it — a Period
requires one to belong to. Most tenants create these once a year, ahead
of time.

## Creating a fiscal year

1. Go to **Fiscal Year** and choose **New**.
2. Enter a Name (e.g. "FY2026"), Start Date, and End Date.
3. Status defaults to **Open**.
4. Save.

## Rules to know

- Name, Start Date, and End Date are required.
- Status is **Open** or **Closed** — but unlike Period's own status, a
  Fiscal Year's Closed status is a bookkeeping label only: it does not,
  by itself, block anything from posting. It's the individual **Period**
  records inside the year — not the year record itself — that the
  ledger actually checks before accepting a posting; see Period's own
  topic for the status that does enforce something.
- Nothing checks that a Fiscal Year's Periods stay inside its own
  Start Date/End Date range, or that Fiscal Years don't overlap — keep
  your own dates consistent.

## What it connects to

Every **Period** references the Fiscal Year it belongs to.
