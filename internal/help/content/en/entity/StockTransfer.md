---
title: Stock Transfer
audience: user
module: purchasing
order: 13
---

A Stock Transfer records one item moving from one Facility to another —
including the in-between state where it has left one location but hasn't
arrived at the other yet.

## When to use it

Create a Stock Transfer whenever you're moving stock between two
facilities — a warehouse to a store, or between two warehouses — and you
want to track it as more than an instant edit, especially if the move
takes real time.

## Recording a transfer

1. Go to **Stock Transfer** and choose **New**.
2. Choose the **Item**, the **From Facility**, and the **To Facility** —
   these must be two different facilities.
3. Enter the **Quantity** — it must be greater than zero.
4. Enter the **Transfer Date**.
5. Optionally add **Notes**.
6. Save. This creates the transfer in **Draft**.
7. Once stock has actually left the source facility, move Status to **In
   Transit**.
8. Once it has arrived at the destination, move Status to **Received**.

## Rules to know

- Item, From Facility, To Facility, Quantity, Transfer Date, and Status
  are all required.
- From Facility and To Facility must be different — a transfer to itself
  is rejected.
- Quantity must be strictly greater than zero.
- The normal path is Draft → In Transit → Received. Cancelling is
  possible from Draft or In Transit, but not once Received — at that
  point stock has actually arrived, and reversing it is a new, opposite
  transfer.
- Recording a Stock Transfer, at any status, does not currently debit or
  credit any Inventory Item quantity by itself — the quantities at the
  source and destination Facility are not automatically adjusted. Track
  the effect on stock levels separately for now if you rely on it.

## What it connects to

A Stock Transfer references one **Item** and two **Facility** records —
the source and the destination.
