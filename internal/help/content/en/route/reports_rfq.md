---
title: RFQ Vendor Comparison
audience: user
module: purchasing
order: 16
---

This report lays out one Request for Quotation's invited vendors side by
side against the lines you asked them to quote, so you can compare their
pricing at a glance before deciding who to buy from.

## When to use it

Open it once vendors have started responding to a Request for Quotation
you sent out, to compare what each one quoted line by line — informing a
decision, not making one: nothing here selects a winner or creates a
Purchase Order for you.

## Opening the report

From an existing **Request for Quotation**'s own form, choose **Compare
Quotes**. There's nothing to compare for an RFQ that hasn't been saved
yet, so the action only appears once the record exists.

## Reading the grid

- One row per requested line (item and quantity), one column per invited
  vendor.
- Each cell shows that vendor's quoted unit price for that line, if they
  quoted it at all.
- **A vendor who hasn't quoted a given line shows "—", never a
  fabricated zero** — a missing quote and a genuine zero-price quote are
  different facts, and this report never blurs them together.
- The lowest *present* price in each row is visually marked — a vendor
  who simply hasn't quoted that line yet is never in the running for
  that mark.
- The footer totals each vendor's own quoted total, but only across the
  lines that vendor actually quoted — it never assumes a missing line
  would have cost that vendor nothing.

## Rules to know

- This report is purely informational. There is no "select winning
  quote" action and no RFQ-to-Purchase-Order conversion here — reading
  it is the whole job; acting on what you read happens elsewhere (for
  example, creating a Purchase Order with the vendor you chose).
- Viewing the report requires read access to the RFQ itself, its Lines,
  its invited Vendors, Quote Lines, and the Items/Parties they
  reference; it's denied as a whole page if any of those is restricted
  for you.
- Loading this report never writes anything — not even an audit row —
  the same as browsing any other read-only record.

## What it connects to

Reads from one **Request for Quotation** and its **Request for
Quotation Line**/**Request for Quotation Vendor**/**Request for
Quotation Quote Line** records, plus **Item** and **Party** for the
human-readable names shown. The **Purchasing Report** (also under
Reports) is the tenant-wide dashboard whose own vendor scorecard
sections summarize across every vendor and every completed order, rather
than one RFQ at a time.
