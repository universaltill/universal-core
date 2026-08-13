---
title: Reorder Rule
audience: user
module: purchasing
order: 14
---

A Reorder Rule is a per-item replenishment policy — the point at which
the purchasing report tells you it's time to buy more of an item, and
how much of a buffer you want to keep on top of that.

## When to use it

Set up a Reorder Rule for any Item you want the purchasing report to
watch and signal on. Without one, that Item never appears in the
report's reorder section, however low its stock gets.

## Setting up a rule

1. Go to **Reorder Rule** and choose **New**.
2. Choose the **Item**.
3. Enter the **Reorder Point** — the stock position at or below which you
   want to be told to reorder.
4. Optionally enter **Safety Stock** — an extra buffer added on top of
   the Reorder Point. Defaults to 0, meaning no buffer.
5. Choose the **Lead-Time Confidence**: P50 or P90. This decides how
   cautious the "order by" guidance on the purchasing report is — P90 (the
   default) assumes a slower, less favourable lead time than the median
   vendor delivery, so it tells you to order earlier; P50 is the
   coin-flip median instead.
6. Save.

## What triggers a signal

The purchasing report compares an item's inventory position — quantity
on hand plus quantity already on open Purchase Orders — against this
rule's Reorder Point plus Safety Stock. Once that position falls to the
combined threshold or below, the item appears in the report's reorder
section with the recommended timing based on your chosen Lead-Time
Confidence. This rule holds only the policy; all of that comparison
happens on the report itself, not here.

## Rules to know

- Item, Reorder Point, and Lead-Time Confidence are all required.
- Reorder Point and Safety Stock cannot be negative.
- One Item is expected to have at most one active rule in practice —
  nothing in the system stops you from creating a second one for the
  same Item, but the report's own behaviour with more than one per item
  isn't something to rely on.

## What it connects to

A Reorder Rule references one **Item**. Its signal, when triggered, is
shown on the purchasing report alongside that Item's current **Inventory
Item** position and any open **Purchase Order** quantity.
