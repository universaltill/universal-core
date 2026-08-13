---
title: Item
audience: user
module: purchasing
order: 1
---

An Item is a sellable or stockable thing — the product or service every
other purchasing record ultimately refers to. A PO Line, a
Request for Quotation, an Inventory Item's stock level: all of them point
back to an Item.

## When to use it

Register an Item before you order it, quote it, or track stock for it —
every reference to "what" in this module (a line, a quote, a stock level)
starts with an Item that already exists.

## Registering an item

1. Go to **Item** and choose **New**.
2. Enter a **SKU** (your own reference code) and a **Name**.
3. Choose the **Type**: Stock (a physical thing you hold inventory for),
   Service (labour or work, nothing to stock), or Non-Stock (something you
   buy and resell or use without tracking a quantity on hand). Defaults to
   Stock.
4. Optionally choose a **Unit of Measure** — each, kilogram, box, whatever
   this item is counted in.
5. Save.

## Rules to know

- SKU, Name, and Type are all required.
- SKU must be unique — it's the natural key a buyer or vendor would
  quote, and a second Item with the same SKU is rejected outright.
- Choosing Service or Non-Stock doesn't change anything else about the
  record today — Inventory Item and Reorder Rule can still reference any
  Item regardless of Type, so a service item left in an inventory report
  is a data-entry choice, not something the system will catch.

## What it connects to

A **PO Line**, **Goods Receipt Line**, **RFQ Line**, **Stock Transfer**,
**Inventory Item**, and **Reorder Rule** can all reference an Item. Its
Unit of Measure comes from the foundation module's Unit of Measure
records.
