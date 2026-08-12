---
title: Unit of Measure
audience: both
module: foundation
order: 10
---

A Unit of Measure is a base unit — each, box, kilogram, litre — that other
parts of the system (inventory, procurement, sales, manufacturing) refer to
when they record a quantity. It exists so a quantity always has a defined,
shared meaning instead of being a bare number.

## When to use it

Set up a Unit of Measure for every distinct unit your business orders,
stocks, or sells in. Most tenants only need to do this once, early on, and
then reference the same set of units everywhere.

## Creating a unit of measure

1. Go to **Unit of Measure** and choose **New**.
2. Enter a short code (for example, "EA" or "BOX") and a full name.
3. Save.

## Rules to know

- Code and name are both required; there is no built-in list of standard
  units — you define exactly the units your business needs.
- A Unit of Measure on its own does not know how to convert to any other
  unit — that relationship is recorded separately with a **UoM Conversion**.

## What it connects to

A **UoM Conversion** links two Units of Measure with a conversion factor.
Once Inventory, Procurement, Sales, or Manufacturing is enabled, items and
order lines reference a Unit of Measure for their quantities.
