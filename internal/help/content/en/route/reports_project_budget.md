---
title: Project Budget: Planned vs. Actual
audience: user
module: projects
order: 5
---

This report compares what a project's Budget Lines planned, category by
category, against what has actually happened so far — a read-only view,
reached from the project itself.

## When to use it

Open it whenever you want to know whether a project is tracking to its
planned budget, one category at a time, rather than only looking at the
single overall Budget figure on the project.

## Opening the report

From an existing project's form, choose **Budget vs Actual**. There is
nothing to open for a project that hasn't been saved yet — a project
needs Budget Lines and, for the Labour category, logged time before
there is anything to compare.

## Reading the numbers

- **Planned** is the sum of that category's Budget Line rows.
- **Actual** is what this system can currently compute for that
  category. Today that is only the **Labour** category — the value of
  every logged Time Entry hour on the project's Tasks, priced at the
  hourly cost rate of whoever logged it.
- Every other category (Materials, Travel, Equipment, Other) shows
  **Not available** for Actual, not a zero. This system has no source
  yet for what was actually spent in those categories — a zero would
  read as "confirmed nothing spent," which would not be true.
- A real, confirmed Actual of zero (labour logged with no cost) still
  shows as **0**, distinctly from **Not available**.
- If some logged hours could not be priced (the person who logged them
  has no cost rate on record), the Labour row says so directly. The
  Actual shown for Labour in that case is still real, but partial — it
  does not include those unpriced hours.

## Rules to know

- This report never lets you edit anything — change Budget Lines or log
  time on the project's Tasks itself, then come back to see the updated
  comparison.
- Viewing this report requires read access to Projects, Budget Lines,
  Tasks, Time Entries, and Employees. It never shows an individual
  person's hourly cost rate directly, only the category total it
  contributes to.

## What it connects to

This report reads from one **Project** and its **Budget Lines**, and
computes Labour actuals from that project's **Tasks**' logged **Time
Entries**, priced against the logging person's **Employee** record.
