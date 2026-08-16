---
title: Status
audience: both
module: foundation
order: 15
---

A Status is one allowed state within a **Status Type**'s lifecycle — for
example, "Draft," "Submitted," or "Approved" within Purchase Order Status.
Which Status values a record can actually move between is defined by
**Status Transition**, not by this record alone.

## When to use it

Like Status Type, you will not usually create these by hand for a record
type Purchasing or another module already sets up for you — they arrive
already seeded. You would add Status records yourself only when defining a
brand-new lifecycle from scratch on a new Status Type.

## Creating a status

1. Go to **Status** and choose **New**.
2. Choose the Status Type it belongs to, and enter a code. The name field
   has one box per available language — fill in at least the primary
   language (required); leaving another language blank just means that
   language falls back to the primary one wherever this status is shown.
3. A sequence number controls its display order only — it has no other
   effect.
4. Mark **Initial** if a new record is allowed to start in this status.
5. Mark **Terminal** as a hint that no further move is expected from this
   status — this is descriptive only; the system does not stop you from
   later adding a Status Transition out of a status marked Terminal.
6. Save.

## Rules to know

- A brand-new record must start in a Status flagged **Initial** — creating
  it with any other status is rejected. A Status Type can have more than
  one Initial status if a record type legitimately has more than one valid
  starting point.
- Moving a record from one status to another only succeeds if a matching
  **Status Transition** row exists for that exact from/to pair — there is no
  such thing as an "allowed by default" move once a lifecycle is in place.
- As an example: Purchase Order Status seeds Draft as its only initial
  status, with Draft → Submitted → Approved → Received as the normal path,
  and Draft, Submitted, or Approved (but not Received) able to move to
  Cancelled. Trying to move a purchase order straight from Draft to
  Received — skipping Submitted and Approved — is rejected, because no
  Status Transition connects them directly.
- A record's Status picker only offers statuses that belong to that
  record type's own Status Type — a status seeded for a different record
  type (a different Status Type) is never offered, even if you can read
  every Status Type in the tenant.

## What it connects to

Every Status belongs to one **Status Type**. **Status Transition** records
name a Status as either their starting or ending point.
