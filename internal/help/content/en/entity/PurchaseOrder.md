---
title: Purchase Order
audience: user
module: purchasing
order: 2
---

A Purchase Order is a committed order to a vendor — what your business
has agreed to buy, from whom, and at what price. It's the buy-side
counterpart to a Sales Order: instead of recording what you sell, it
records what you buy.

## When to use it

Create a Purchase Order once you've decided to buy from a vendor, not
while you're still comparing prices — if you're still soliciting pricing
from more than one vendor, use a **Request for Quotation** first.

## Creating a purchase order

1. Go to **Purchase Order** and choose **New**.
2. Enter a **PO Number** — your own reference for this order. It must be
   unique across every Purchase Order you've created.
3. Choose the **Vendor** — only Party records already holding the vendor
   role can be picked here; a customer-only Party won't appear.
4. Enter the **Order Date** and, optionally, a **Promised Delivery Date**
   (the date the vendor committed to at order time) — this can't be
   earlier than the Order Date.
5. Choose a **Currency** if the vendor trades in something other than
   your default.
6. Add lines in the **Lines** section: for each item, the quantity and
   unit price. **Line Total is not calculated for you** — enter it
   yourself for each line.
7. Save.

The **Total** field recalculates automatically every time you open the
form, from the sum of every line's Line Total — you don't need to add the
lines up by hand, but you do need to save at least once after adding
lines for the recalculated total to be recorded.

## Lead-time stages

The **Lead-time stages** section records when this order actually moved
through sourcing — Sourced, Production Started, Production Ready,
Shipped, Customs Cleared, and Received. All are optional and entered by
hand, each one no earlier than the stage before it, and an order in
transit will normally have only a prefix of them filled in. These feed
the purchasing report's lead-time figures; nothing fills them in for you
automatically, including Received — recording a stage here does not by
itself change the order's Status below.

## Rules to know

- Status starts at **Draft** and can only move along the path a Status
  Transition allows: Draft → Submitted → Approved → Received is the
  normal path. Cancelling is possible from Draft, Submitted, or Approved,
  but not once an order has moved to Received — at that point goods have
  actually arrived, and reversing that is a return, not a status edit.
- PO Number, Vendor, Order Date, and Status are all required, and PO
  Number must be unique.
- The Vendor must hold the vendor role on their Party record. Picking a
  Party that has never been marked as a vendor is rejected.
- Total cannot be negative.
- Marking a Purchase Order Received here is just a status change — it
  does not by itself record what physically arrived. Use **Goods
  Receipt** for that; a single order is often received in more than one
  delivery.

## What it connects to

Each Purchase Order has one or more **PO Line** rows (its
Lines section). A **Goods Receipt** records physical deliveries against
it, and a **Vendor Invoice** bills against it and is matched against what
was actually received. The **Vendor** is a Party holding the vendor
role. A Purchase Order can be exported as a UBL order document, in any
status, via the form's "Download UBL file" action. The purchasing report
(under Reports) summarizes vendor spend and lead times across every
Purchase Order.
