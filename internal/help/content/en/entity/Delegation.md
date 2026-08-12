---
title: Delegation
audience: admin
module: foundation
order: 21
---

A Delegation lets one user (the delegate) stand in for another user's
(the delegator's) approval standing while it is active — for example, so
approvals do not stall while someone is travelling or on leave. It grants
only approval eligibility, nothing else: it does **not** hand over the
delegator's Role, entity access, admin status, or anything else access
control normally governs.

## When to use it

Set up a Delegation when a specific person needs to be able to approve
things on someone else's behalf for a defined period — or indefinitely,
until you turn it off.

## Creating a delegation

1. Go to **Delegation** and choose **New**.
2. Enter the delegator's and the delegate's user identifiers.
3. Optionally set an end date — the delegation stays active through the end
   of that day. Leave it blank for a delegation with no fixed end.
4. Save.

## Rules to know

- A Delegation only ever affects one hop: if A delegates to B, and B has
  separately delegated to C, C does **not** inherit A's standing through B.
  Each delegation is a direct, one-step relationship.
- To end a delegation before its end date (or an indefinite one), mark it
  **Revoked** rather than deleting it — this keeps a visible history of who
  was delegated to and when, instead of erasing the record.
- Delegating to yourself is allowed by the form but has no effect — it
  would only grant you standing you already have.
- Delegation is one of the record types the same admin-only lock described
  on **Role**'s own topic applies to — once that lock has activated, only
  `tenant_admin` can create or edit a Delegation, same as Permission or
  Field Permission. (Creating a Delegation does not itself activate the
  lock; only a Permission/Field Permission row or a `tenant_admin` grant
  does — see **Role** for exactly when.)

## What it connects to

A Delegation stands on its own — it references users by their identifier,
not by a Role or Party record. Once Workflow approvals are in use, an
approval check consults active Delegation rows to see who else may act for
the original approver.
