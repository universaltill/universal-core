---
title: Time Entry
audience: user
module: projects
order: 3
---

A Time Entry is a record of hours someone worked on a Task, on a given
date. This is the raw log everything else about a task's actual effort
is built from.

## When to use it

Log a Time Entry whenever work happens against a task — at the end of
the day, or however often your team records time. Each entry is one
person, one task, one date.

## Logging time

1. Go to **Time Entry** and choose **New**.
2. Choose the **Task** and the **Employee** who did the work — only a
   Party holding the employee role can be picked here.
3. Set the **Date** and the number of **Hours**.
4. Leave **Billable** checked if this time should be billable to the
   customer, or clear it if not. It's checked by default.
5. Optionally add **Notes**.
6. Save.

## Rules to know

- Task, Employee, Date, and Hours are all required.
- Hours must be between 0 and 24 — one entry cannot exceed a calendar
  day. Log separate entries for separate days rather than one entry
  spanning several.
- Billable defaults to checked, but it's just a flag on the entry —
  whether an hour actually becomes an invoice line is decided elsewhere,
  not by this record alone.

## What it connects to

A Time Entry belongs to a **Task** and records the **Employee** who
logged it. It appears in the task's own Logged Time list, and through
the task, to the task's Project.
