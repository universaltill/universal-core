---
title: Facility
audience: user
module: purchasing
order: 11
---

A Facility is a physical or logical stock location — a warehouse, a shop
floor, or a virtual bucket for stock you're not tracking anywhere more
specific. It's where inventory actually is.

## When to use it

Register a Facility before you receive goods into it, transfer stock
to or from it, or track inventory at it — every stock quantity in this
module is tied to a specific Facility, not just an Item on its own.

## Registering a facility

1. Go to **Facility** and choose **New**.
2. Enter a **Code** (your own reference) and a **Name**.
3. Choose the **Type**: Warehouse, Store, or Virtual. Defaults to
   Warehouse.
4. Optionally choose an **Address**.
5. **Active** defaults to on.
6. Save.

## Rules to know

- Code, Name, and Type are all required.
- Code is meant to be a natural, human-quoted reference, but nothing in
  the system currently stops two Facilities from sharing the same Code.
- Turning **Active** off is how you retire a facility without deleting
  it — it should stop appearing in pickers for new activity, but any
  stock history that already points at it stays intact. This does not
  move or clear any stock recorded at that facility.

## What it connects to

A **Goods Receipt** records which Facility goods arrived at. An
**Inventory Item** row is a quantity at one specific Item-and-Facility
pair. A **Stock Transfer** moves stock from one Facility to another.
