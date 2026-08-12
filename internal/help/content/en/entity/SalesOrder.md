---
title: Sales Order
audience: user
module: sales
order: 1
---

A Sales Order is a committed order from a customer — what they agreed to
buy, at what price, and when. It is the sell-side counterpart to a
Purchase Order: instead of recording what your business buys, it records
what your business sells.

## When to use it

Create a Sales Order once a customer's order is confirmed, not while
you're still discussing it — there is no separate "quotation" stage yet,
so a Sales Order in Draft status is the closest thing to a working
estimate.

## Creating a sales order

1. Go to **Sales Order** and choose **New**.
2. Enter an SO Number (your own reference for this order).
3. Choose the **Customer** — only Party records already holding the
   customer role can be picked here; a vendor-only Party won't appear.
4. Enter the Order Date and, if this customer trades in a currency other
   than your default, the Currency.
5. Add lines in the **Lines** section: for each item, the quantity and
   unit price. **Line Total is not calculated for you** — enter it
   yourself for each line (quantity × unit price, if that's how you
   price it).
6. Save.

The **Total** field recalculates automatically every time you open the
form, from the sum of every line's Line Total — you never need to add
the lines up by hand, but you do need to save at least once after adding
lines for the recalculated total to be recorded.

## Rules to know

- Status starts at **Draft** and can only move along the path a Status
  Transition allows: Draft → Confirmed → Fulfilled → Invoiced is the
  normal path. Cancelling is possible from Draft or Confirmed, but not
  once an order has moved to Fulfilled or Invoiced — at that point
  something has actually shipped or been billed, and reversing it is a
  return or credit note, not a status edit.
- SO Number, Customer, Order Date, and Status are all required.
- The Customer must hold the customer role on their Party record. Picking
  a Party that has never been marked as a customer is rejected.
- Total cannot be negative.
- Saving a Sales Order does not, by itself, post anything to the ledger —
  nothing is owed until you issue a **Customer Invoice** against it (see
  that topic for what issuing actually does).

## What it connects to

Each Sales Order has one or more **SO Line** rows (its Lines section).
A **Customer Invoice** references the Sales Order it bills against. The
**Customer** is a Party holding the customer role. A Sales Order can be
exported as a UBL order document, in any status, via the form's
"Download UBL file" action.
