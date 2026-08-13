---
title: Fixed Asset
audience: user
module: assets
order: 1
---

A Fixed Asset is a piece of property your organization owns and
depreciates over time — equipment, a vehicle, anything with a cost, a
useful life, and a value that declines as it's used. It's the record
its depreciation schedule and maintenance history are built around.

## When to use it

Register a Fixed Asset when you acquire something you'll depreciate:
enter what it cost, how long you expect to use it, and which accounts
its depreciation should post to.

## Registering an asset

1. Go to **Fixed Asset** and choose **New**.
2. Enter an Asset Number (your own reference) and a Name.
3. Optionally set a **Location**.
4. Enter the **Acquisition Date**, **Currency**, **Cost**, and
   (optionally) **Salvage Value** — what it will still be worth at the
   end of its useful life. Salvage Value defaults to 0.
5. Enter the **Useful Life (months)** — the depreciation term, in
   months rather than years.
6. **Depreciation Method** defaults to, and today only offers,
   Straight Line.
7. Choose the three **Posting Accounts**: Asset Account (where the
   original cost is held), Depreciation Expense Account, and
   Accumulated Depreciation Account.
8. Save.

## The depreciation schedule

The **Depreciation Schedule** section on the asset form lists the
asset's periods — one row per month, each with the amount to depreciate
and the resulting book value. **This section does not fill itself in
automatically**: schedule rows are records like any other, added and
edited the same way you'd add any composition child, and it's this
section's rows — not the asset's cost and useful life alone — that a
depreciation posting run reads and posts from once the asset is In
Service. If a row is wrong, correct it directly here rather than
re-entering the whole schedule.

## Rules to know

- Asset Number, Name, Acquisition Date, Cost, Useful Life, Depreciation
  Method, and Status are all required. Cost, Salvage Value, and Useful
  Life cannot be negative.
- The three Posting Accounts are not enforced as required fields, but a
  depreciation posting run silently skips every due row for an
  In Service asset that's missing any one of them (logged, not
  surfaced to you) — leave all three set once the asset is in service.
- Status models an asset's real life, not a document approval: **Draft**
  is the only starting point (registered but not yet depreciating).
  **In Service** is where depreciation posts. **Fully Depreciated** is
  reached once the schedule is exhausted. **Disposed** and **Written
  Off** are both reachable from Draft, In Service, or Fully Depreciated
  — an asset can be sold or scrapped at any point in its life — and both
  are final: nothing returns from them, since un-disposing an asset is a
  new acquisition, not a status change.

## What it connects to

A Fixed Asset can reference a **Currency** and three **Account**
records for posting. Its **Depreciation Schedule** rows belong to it
directly. Its **Maintenance History** shows every **Maintenance Order**
raised against it — a read-only list on the asset form; maintenance
orders are their own independent records, not edited from here.
