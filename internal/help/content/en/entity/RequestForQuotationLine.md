---
title: RFQ Line
audience: user
module: purchasing
order: 8
---

An RFQ Line is one item and quantity you're asking vendors to price —
one row of an RFQ's Lines section. Unlike a PO Line, it carries no price
of its own: the price is exactly what you're requesting.

## When to use it

Added from within a Request for Quotation's own **Lines** section, one
row per item you want vendors to quote on.

## Adding a line

1. From a Request for Quotation's Lines section, add a new row.
2. Choose the **Item** and enter the **Quantity**.
3. Save.

Each invited vendor's price for this line is recorded separately, as a
**Vendor Quote Line** — this record only says what's being
asked for, not what anyone quoted.

## Rules to know

- Request for Quotation, Item, and Quantity are all required.
- Quantity cannot be negative.

## What it connects to

An RFQ Line belongs to one **Request for Quotation** and
references one **Item**. Each vendor's quoted price against this specific
line is its own **Vendor Quote Line** record — one per
vendor who responds.
