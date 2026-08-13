---
title: Maintenance Order
audience: user
module: assets
order: 3
---

A Maintenance Order is service or repair work against a Fixed Asset —
what was scheduled, what actually happened, and what it cost.

## When to use it

Raise a Maintenance Order whenever an asset needs service: a routine
check, a repair, or an inspection. Recording it here builds the asset's
maintenance history, separate from its depreciation.

## Raising a maintenance order

1. Go to **Maintenance Order** and choose **New**.
2. Enter an **Order Number** and choose the **Asset**.
3. Choose the **Type** — Preventive, Corrective, or Inspection. It
   defaults to Corrective.
4. Optionally add a **Description**.
5. Set the **Scheduled Date**.
6. Optionally choose a **Service Provider** (the Party doing the work)
   and a **Currency**.
7. Save. Once the work is actually done, come back and set the
   **Completed Date** and the **Cost**.

## Rules to know

- Order Number, Asset, Type, Scheduled Date, and Status are required.
  Cost defaults to 0 and cannot be negative.
- **Completed Date has no required relationship to Scheduled Date** —
  work finishing ahead of schedule is a good outcome, not an error, so
  it's never rejected for being earlier than planned.
- Status: **Scheduled** → **In Progress** → **Completed** is the normal
  path. **Cancelled** is reachable from Scheduled or In Progress, but
  not from Completed — work that was actually done can't be undone by a
  status change.
- A Maintenance Order's cost is not added to its asset's depreciable
  cost or book value. Capitalizing a repair into an asset's cost is a
  deliberate accounting decision this record does not make for you.

## What it connects to

A Maintenance Order belongs to a **Fixed Asset** and can reference a
**Service Provider** (Party) and a **Currency**.
