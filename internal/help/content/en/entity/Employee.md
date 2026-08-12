---
title: Employee
audience: admin
module: hr
order: 1
---

An Employee record is one employment — not a person. The person
themselves is a **Party** record; this entity carries only what's
specific to the employment itself: which position, which department,
when it started, and whether it's still active. Keeping the two
separate means the same person can have two employment records over
time (a rehire) without ever duplicating their name or contact details,
and without the first employment's history being overwritten by the
second.

## When to use it

Create an Employee record when you hire someone into a role — after the
person already exists as a Party (create the Party first if they
don't). If someone leaves and later comes back, create a **new**
Employee record for the rehire rather than editing the old one back to
Active — the old record's dates and history should stay exactly as
they were.

## Creating an employee record

1. Go to **Employee** and choose **New**.
2. Enter an **Employee Number** (your own reference, separate from the
   Party's own identity) and choose the **Person** (the Party).
3. Optionally set the **Position** and **Department**.
4. Enter the **Hire Date**.
5. Optionally enter a **Cost Rate**, in the Compensation section, if
   you track labour cost against this employment (used, for example,
   when pricing logged project time).
6. Save.

## Rules to know

- Employee Number, Person, and Hire Date are required. Status defaults
  to **Probation**.
- Status is a lifecycle: Probation → Active, and Active can move to
  **On Leave** and back again (extended leave is not a termination).
  **Terminated** is reachable from any of the three live states, and is
  a dead end — nothing moves out of it. A rehire is always a new
  Employee record, never a reopened old one.
- End Date, if set, must not be before Hire Date. Cost Rate, if set,
  cannot be negative.
- Cost Rate has no default — an Employee record with no rate set is
  different from one with a rate of zero. A missing rate means "not
  known," and anything that prices this employee's time treats that as
  incomplete information, not as free labour. If you don't know the
  rate, leave the field empty rather than entering 0.
- The **Leave History** section lists this employment's Leave Requests
  — a read-only view of decisions that outlive the employment itself.

## What it connects to

Every Employee references the **Person** — intended to be a Party
holding the Employee role, though (like Case's Customer field) nothing
actually checks that at save time — and, optionally, a **Position** and
**Department**. **Leave
Request** and **Attendance Record** both reference the Employee, not
the Party directly, because they're about a specific employment —
someone with two employments over time has two separate leave and
attendance histories.
