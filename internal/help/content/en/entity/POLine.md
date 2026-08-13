---
title: PO Line
audience: user
module: purchasing
order: 3
---

A PO Line is one ordered item, its quantity, and its price —
one row of a Purchase Order's Lines section. Every Purchase Order needs
at least one to actually order anything.

## When to use it

Almost always added from within a Purchase Order's own **Lines** section,
one row per item being ordered. It can also be created or edited on its
own — useful for a bulk import — but it's meaningless without a Purchase
Order to belong to.

## Adding a line

1. From a Purchase Order's Lines section, add a new row (or go to
   **PO Line** and choose **New**, then pick the Purchase
   Order it belongs to).
2. Choose the **Item**.
3. Enter the **Quantity** and **Unit Price**.
4. Enter the **Line Total** yourself — it is not calculated from
   Quantity × Unit Price for you.
5. Save.

The parent Purchase Order's own Total recalculates from every line's Line
Total the next time that order is opened and saved.

## Rules to know

- Purchase Order, Item, Quantity, and Unit Price are all required.
- Quantity and Unit Price cannot be negative.
- Nothing stops a Line Total that doesn't match Quantity × Unit Price —
  it's a separate field you're trusted to fill in correctly.

## What it connects to

Every PO Line belongs to one **Purchase Order** and
references one **Item**. A **Goods Receipt Line** references the
specific PO Line it was received against, and its Unit Price
is what a receipt's ledger posting and a Vendor Invoice's matching both
use.
