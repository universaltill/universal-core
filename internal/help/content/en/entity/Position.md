---
title: Position
audience: both
module: foundation
order: 9
---

A Position is a seat or role within your org chart — "Warehouse Supervisor,"
"Finance Manager" — distinct from the person who occupies it. A Position can
exist, and sit in a reporting line, before anyone is actually assigned to
it.

## When to use it

Create Positions to model your organization's reporting structure ahead of,
or independently of, hiring or assigning people to them. Once the HR module
is enabled, an Employee record occupies a Position; Position itself does
not track who currently holds it.

## Creating a position

1. Go to **Position** and choose **New**.
2. Enter a title.
3. Optionally choose the Department it belongs to, and the Position it
   reports to.
4. Save.

## Rules to know

- Only the title is required. Department is optional on purpose — a
  company-wide or matrix position (a CFO who does not report through a
  single department, for example) does not need a department at all.
- "Reports to" lets you build a reporting chain of Positions independent of
  Departments — a Position can report to another Position in a different
  Department, or in none.
- A Position and an Employee are different things: this record represents
  the seat, not the person in it.

## What it connects to

A Position can reference a **Department** and another **Position** (the one
it reports to). Once HR is enabled, Employee records reference a Position
to show who occupies it.
