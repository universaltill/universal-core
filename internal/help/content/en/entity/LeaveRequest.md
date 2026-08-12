---
title: Leave Request
audience: both
module: hr
order: 2
---

A Leave Request records time off asked for and decided — Annual, Sick,
Unpaid, Parental, or another category — against one specific
employment.

## When to use it

Submit a Leave Request whenever you need time off recorded and
approved. It belongs to your **Employee** record (the specific
employment), not to you as a person, so if you've ever had more than
one employment record, make sure you're working from the right one.

## Requesting leave

1. Go to **Leave Request** and choose **New**.
2. Enter a **Request Number** and choose the **Employee** (your current
   employment record).
3. Choose the **Leave Type** — Annual, Sick, Unpaid, Parental, or
   Other.
4. Enter the **Start Date** and **End Date**, and the number of
   **Days** this covers.
5. Optionally enter a **Reason**.
6. Save, then submit it for approval by moving Status forward.

## Rules to know

- Request Number, Employee, Leave Type, Start Date, End Date, and Days
  are all required. Status defaults to **Draft**.
- Status is an approval chain: Draft → Submitted → Approved is the
  normal path, and Submitted can also move to **Rejected**.
  **Cancelled** is reachable from Draft, Submitted, or even Approved —
  withdrawing already-approved leave is an ordinary thing to do, since
  unlike an approved purchase order, no money has moved yet.
- Days cannot be negative. There is no maximum enforced here — how many
  days you're entitled to is a policy question for your organization,
  not something this form checks.
- End Date must not be before Start Date. Days is entered directly
  rather than calculated from the date span, because half-days, public
  holidays, and non-working days make a straight date subtraction wrong
  more often than not — enter the actual number of days this request
  should count.

## What it connects to

Every Leave Request references the **Employee** (the specific
employment) it belongs to, and appears in that Employee's own **Leave
History** section.
