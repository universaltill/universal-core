---
title: Status Transition
audience: both
module: foundation
order: 16
---

A Status Transition is one legal move in a **Status Type**'s lifecycle: from
one **Status** to another. If no Status Transition row connects two
statuses, moving a record directly between them is not allowed — the
absence of a row is what makes a move illegal, not a separate rule you have
to configure.

## When to use it

As with Status Type and Status, most tenants never create these by hand —
a module like Purchasing seeds the transitions its own record types need
when it is enabled. You would add Status Transition rows yourself only when
building a brand-new lifecycle on a new Status Type.

## Adding a transition

1. Go to **Status Transition** and choose **New**.
2. Choose the Status Type, the **From** status, and the **To** status.
3. Optionally name a workflow that should gate this move — recorded for
   future use, but not yet enforced by the system on its own.
4. Save.

## Rules to know

- Direction matters: a row from Draft to Approved does not also allow
  Approved back to Draft — add a second row if the reverse move should be
  legal too.
- **From** and **To** must each belong to the Status Type chosen on this
  row — you cannot point either one at a status from a different Status
  Type's lifecycle.
- A confirmed example from Purchase Order Status: Draft → Submitted,
  Submitted → Approved, and Approved → Received form the normal path.
  Draft → Cancelled, Submitted → Cancelled, and Approved → Cancelled are
  also legal, letting an order be cancelled at any point before it's
  received — but there is deliberately no transition out of Received or out
  of Cancelled, since both are treated as final: a purchase order that has
  already been received or cancelled is not edited back into another
  status, a new document is created instead.
- Attempting an update that sets a status with no matching Status
  Transition row from the record's current status is rejected outright,
  with an error naming the illegal from/to pair.

## What it connects to

Every Status Transition names a **Status Type** and the two **Status**
records — the starting one and the ending one — it connects.
