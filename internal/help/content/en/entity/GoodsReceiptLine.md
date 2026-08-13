---
title: Goods Receipt Line
audience: user
module: purchasing
order: 5
---

A Goods Receipt Line is one received item and quantity, against a
specific PO Line — one row of a Goods Receipt's Lines
section, and the record that actually credits inventory and posts to the
ledger.

## When to use it

Almost always added from within a Goods Receipt's own **Lines** section,
one row per item actually received in that delivery.

## Adding a line

1. From a Goods Receipt's Lines section, add a new row.
2. Choose the **PO Line** this delivery is against, and the
   **Item** (normally the same item as that line's own).
3. Enter **Qty Received**.
4. Optionally, if you inspect what arrived, enter **Qty Accepted** and
   **Qty Rejected** — how much passed inspection and how much didn't.
   Leave both blank if you're not recording a quality split for this
   delivery.
5. Save. This immediately credits inventory and posts to the ledger — see
   Goods Receipt's own topic for exactly what happens.

## Rules to know

- Goods Receipt, PO Line, Item, and Qty Received are all
  required. Qty Received cannot be negative.
- Qty Accepted and Qty Rejected must both be set or both be left blank —
  recording one without the other is rejected.
- When both are set, Qty Accepted + Qty Rejected must equal Qty Received.
  A line that fails this check is rejected outright, including on a later
  edit — correct the numbers and save again.
- Editing a line's quantities after it was first saved does not re-post
  or reverse anything on the ledger. Only the quality check (accepted +
  rejected must equal received) is re-checked on an edit.

## What it connects to

A Goods Receipt Line belongs to one **Goods Receipt** and references the
**PO Line** and **Item** it was received against. Saving one
credits the **Inventory Item** for that item at the receipt's Facility,
and posts a journal entry using the referenced PO Line's Unit
Price.
