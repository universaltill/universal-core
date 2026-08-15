---
title: Purchasing Report
audience: user
module: purchasing
order: 15
---

The Purchasing Report is a single, read-only dashboard over purchasing
and stock-intelligence data — purchase order status and vendor spend,
current stock levels, supplier performance, and reorder signals — all on
one page.

## When to use it

Open it whenever you want an at-a-glance view of purchasing activity and
stock health, rather than digging through individual Purchase Order or
Item records one at a time.

## Opening the report

Choose **Reports** from the Purchasing module menu, or go directly to
`/reports/purchasing`. There is nothing to configure — it always reflects
the tenant's current data.

## The sections, in order

- **Purchase Orders by Status** — order counts and total value, grouped
  by status.
- **Top Vendors by Spend** — vendors ranked by total Purchase Order
  value.
- **Stock Summary** — items tracked, total quantity on hand, and total
  quantity available to promise, across every facility.
- **Stockout Risk** — items with nothing left available to promise,
  linking through to each item's own record.
- **Supplier Lead Times** — for each vendor, P50 and P90 order-to-receipt
  lead time in days, computed from that vendor's own completed Purchase
  Orders.
- **On-Time Delivery** — the share of completed orders received on or
  before their promised delivery date, per vendor.
- **Quality** — the accepted-vs-rejected rate from recorded Goods Receipt
  Line inspection outcomes, per vendor.
- **Reorder Signals** — items whose current inventory position has
  dropped to or below their Reorder Rule's threshold, with an expected
  lead time so you know roughly how long a new order would take.

## Rules to know

- **Every statistic needs at least 2 completed data points before it
  shows a number at all** — Supplier Lead Times, On-Time Delivery, and
  Quality all show **Insufficient history** below that, rather than a
  misleadingly confident single-sample percentage.
- **A reorder signal is position-based, not just on-hand-based**: the
  inventory position is quantity on hand *plus* the undelivered quantity
  on any open Purchase Order for that item. An item with a large open PO
  already inbound will not fire just because what's physically on the
  shelf right now is low — the goods are already coming.
- A signal fires when that position drops to or below the item's Reorder
  Rule **Reorder Point** (plus its Safety Stock, if set).
- The expected lead time shown next to a firing signal uses the P50 or
  P90 figure (per the Reorder Rule's own confidence setting) of the
  vendor on that item's *most recent* Purchase Order. If that vendor
  doesn't have enough history of its own, the report falls back to the
  overall figure across every vendor — labeled **(all suppliers)** so
  you never mistake a fleet-wide number for that one vendor's own track
  record.
- This report never lets you act on anything shown — no reordering, no
  editing. It's read-only; the fastest next step from a stockout or
  reorder row is following its link to the Item itself.
- Viewing the report requires read access to every entity type it draws
  from; it's denied as a whole page, not partially redacted, if any one
  of them is restricted for you.

## What it connects to

Reads from **Purchase Order**, **PO Line**, **Goods Receipt**, **Goods
Receipt Line**, **Item**, **Inventory Item**, **Party** (vendors), and
**Reorder Rule** — nothing here is editable from this page; every number
traces back to those records elsewhere in the system. The related
**RFQ Vendor Comparison** report covers vendor pricing for one specific
Request for Quotation, which this page does not.
