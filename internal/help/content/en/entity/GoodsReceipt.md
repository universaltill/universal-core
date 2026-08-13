---
title: Goods Receipt
audience: user
module: purchasing
order: 4
---

A Goods Receipt records that goods physically arrived against a Purchase
Order — where they arrived, when, and (through its lines) what and how
much. A single Purchase Order is often received in more than one
delivery, so recording a receipt is a repeatable event, not just a status
change on the order itself.

## When to use it

Create a Goods Receipt each time a delivery actually arrives against a
Purchase Order — including a partial delivery. There's no need to wait
for the whole order to show up before recording what has.

## Recording a receipt

1. Go to **Goods Receipt** and choose **New**.
2. Choose the **Purchase Order** this delivery is against.
3. Enter the **Received Date**.
4. Choose the **Facility** the goods arrived at — this is required, and
   it's what determines which Inventory Item record gets credited.
5. Add lines in the **Lines** section: for each item actually received,
   the quantity. Optionally, if you inspect what arrived, record **Qty
   Accepted** and **Qty Rejected** — see below.
6. Save.

## What saving a line actually does

The moment a line is saved, two things happen at once, for that line
only:

- **Inventory goes up.** The received quantity is credited to the
  Inventory Item record for that item at that facility (creating one if
  none exists yet).
- **A journal entry posts**, debiting Inventory and crediting Accounts
  Payable for the line's value (quantity × the PO Line's Unit
  Price). A line with no value to post (for example, a free sample) still
  credits inventory even though nothing posts to the ledger.

This happens once, when the line is first saved — there's no separate
"post" step, and there's no un-posting a line once it's been received.

## Rules to know

- Purchase Order, Received Date, and Facility are all required on the
  header; a line's Item and Qty Received are required on each line.
- Qty Accepted and Qty Rejected are optional — most receipts don't record
  them at all. If you record either one, you must record both, and they
  must add up to Qty Received: they're a quality split of what arrived,
  not a second, independent count.
- A Goods Receipt has no status of its own — recording one is itself the
  event; there's no draft or approval stage to move it through.
- Receiving against a Purchase Order does not change that order's own
  Status field — moving a Purchase Order to Received is a separate,
  manual step.

## What it connects to

A Goods Receipt belongs to one **Purchase Order** and has one or more
**Goods Receipt Line** rows. Its lines are what a **Vendor Invoice**'s
matching compares against, and what credits an **Inventory Item**'s
quantity at the chosen **Facility**.
