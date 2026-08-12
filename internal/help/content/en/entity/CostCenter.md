---
title: Cost Center
audience: admin
module: finance
order: 5
---

A Cost Center is a tag for where spending or revenue belongs
organizationally — a department, a project, a location — independent of
which Account it posted to. More than one Cost Center can spend from the
same Account (e.g. "5200 Operating Expenses"); this record is how you'd
eventually tell them apart.

## When to use it

Set these up if your business tracks costs or revenue by department,
project, or location and wants to report on that breakdown separately
from the chart of accounts itself.

## Creating a cost center

1. Go to **Cost Center** and choose **New**.
2. Enter a Code and Name.
3. Optionally enter a Type — a free-text label of your own choosing
   (e.g. "Department", "Location") to group Cost Centers by kind.
4. Save.

## Rules to know

- Code and Name are required; Type is optional free text with no fixed
  list of values.
- Code is not schema-enforced unique — keep your own numbering
  consistent.
- Nothing in this first release of the product actually assigns a Cost
  Center to a journal entry, invoice, or order line yet — this record
  exists as reference data ahead of that wiring, not as something you'll
  see reflected in a posting today.

## What it connects to

Nothing yet reads a Cost Center automatically — it stands alone as
reference/master data for now.
