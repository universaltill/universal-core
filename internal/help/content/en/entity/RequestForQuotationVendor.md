---
title: RFQ Vendor
audience: user
module: purchasing
order: 9
---

An RFQ Vendor is one vendor invited to quote against a
Request for Quotation — one row of an RFQ's Vendors section.

## When to use it

Added from within a Request for Quotation's own **Vendors** section, one
row per vendor you're inviting to quote.

## Adding a vendor

1. From a Request for Quotation's Vendors section, add a new row.
2. Choose the **Vendor**.
3. Save.

Inviting a vendor here doesn't send anything by itself — recording the
invitation and actually contacting the vendor are two separate steps; see
Request for Quotation's own topic.

## Rules to know

- Request for Quotation and Vendor are both required.
- Unlike a Purchase Order's own Vendor field, this picker does not check
  that the Party actually holds the vendor role — any Party can be
  picked here, so choose carefully.

## What it connects to

An RFQ Vendor belongs to one **Request for Quotation** and references
one **Party** (the vendor). Once that vendor responds, their price for
each requested line is recorded as a separate **Vendor Quote Line**, not
on this record.
