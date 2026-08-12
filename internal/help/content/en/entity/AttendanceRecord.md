---
title: Attendance Record
audience: admin
module: hr
order: 3
---

An Attendance Record is one employment's attendance for one day — hours
worked, and where the number came from: a badge/clock system, an
imported timesheet, or a manual correction.

## When to use it

Use Attendance Record to record or correct how many hours someone
worked on a given day. This is a record of what happened, not a
request anyone approves — for time off that needs approval, use
**Leave Request** instead.

## Recording attendance

1. Go to **Attendance Record** and choose **New**.
2. Choose the **Employee** (the specific employment this attendance is
   against) and the **Date**.
3. Enter the **Hours Worked**.
4. Choose the **Source** — Clock, Timesheet, or Manual — recording
   where this number actually came from.
5. Optionally add **Notes**, useful for explaining a manual correction.
6. Save.

## Rules to know

- Employee, Date, Hours Worked, and Source are all required. Source is
  pre-filled with **Timesheet** on the form, but it is still a required
  field — a record created another way (import, API) with no source set
  is rejected, not silently defaulted.
- **Only one Attendance Record is allowed per employee per day** —
  saving a second row for the same Employee and Date is rejected.
  Correct an existing day by editing its record, not by adding another
  one.
- Hours Worked must be between 0 and 24.
- There is no approval step and no status field — this entity has no
  lifecycle, unlike Leave Request. It records a fact, and correcting a
  wrong fact means editing the existing row.

## What it connects to

Every Attendance Record references the **Employee** (the specific
employment) it's against. It is not shown as a related list on the
Employee form itself — look it up from its own list, filtered by
Employee, instead.
