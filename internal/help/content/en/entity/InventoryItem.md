---
title: Inventory Item
audience: user
module: purchasing
order: 12
---

An Inventory Item is a stock quantity — how much of one Item you have on
hand, and how much is available to promise, at one specific Facility.
Most of the time you won't create these directly; they're kept up to
date for you as goods are received.

## When to use it

You rarely need to create one by hand. Recording a **Goods Receipt**
against an item already creates or updates the matching Inventory Item
for you, crediting it by the quantity received. Create or edit one
directly only to set an opening balance, or to correct a quantity by
hand.

## Viewing or adjusting a quantity

1. Go to **Inventory Item**.
2. Find the row for the **Item** and **Facility** you're looking for (or
   choose **New** to record a stock level that doesn't exist yet).
3. **Qty On Hand** is the physical quantity actually at that facility.
   **Qty Available to Promise** is what's free to commit to new demand —
   in this system the two move together, since nothing yet reserves
   stock separately against sales orders.
4. Save.

## Rules to know

- Item, Facility, Qty On Hand, and Qty Available to Promise are all
  required.
- Qty On Hand cannot be negative. Qty Available to Promise can — a real,
  meaningful state when demand has outstripped what's on hand and
  incoming.
- Each (Item, Facility) pair must have exactly one Inventory Item row —
  a second row for the same pair is rejected outright. Goods receipts
  always update the existing row rather than creating a second one for
  a pair that already has stock.
- Facility is always required — there's no "stock with no location" in
  this system; every quantity belongs to a real Facility.

## What it connects to

An Inventory Item references one **Item** and one **Facility**. A
**Goods Receipt Line** credits it on receipt. A **Reorder Rule** for the
same Item compares its position against this quantity to decide whether
to signal a reorder.
