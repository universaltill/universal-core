---
title: UoM Conversion
audience: both
module: foundation
order: 11
---

A UoM Conversion is a conversion factor between two **Unit of Measure**
records — for example, one box equals twelve each. It is what lets the
system convert a quantity from a stocking unit to an ordering or selling
unit and back.

## When to use it

Create a UoM Conversion whenever you have two units that need to convert
into each other — most commonly, a bulk unit (a box, a pallet) and the
individual unit it contains.

## Creating a conversion

1. Go to **UoM Conversion** and choose **New**.
2. Choose the **From** unit and the **To** unit.
3. Enter the factor — the number you multiply a quantity in the From unit by
   to get the equivalent quantity in the To unit (one box at a factor of 12
   converts to 12 each).
4. Save.

## Rules to know

- The factor must be zero or greater; a negative factor is rejected.
- The From→To direction is a convention this system expects you to follow
  consistently — it is not double-checked against how the conversion is
  actually used elsewhere, so enter the From and To units the same way each
  time (bulk unit first, individual unit second is the common pattern).
- A factor of zero is technically accepted even though it collapses every
  converted quantity to zero — double-check the value you enter.

## What it connects to

A UoM Conversion references two **Unit of Measure** records. It has no
other connections — items and order lines reference their Unit of Measure
directly and rely on a UoM Conversion existing when a quantity needs to be
translated between units.
