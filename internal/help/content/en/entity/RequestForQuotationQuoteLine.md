---
title: Vendor Quote Line
audience: user
module: purchasing
order: 10
---

A Vendor Quote Line is one vendor's quoted price for one
RFQ Line — the record behind the Compare Quotes report.
There's no vendor self-service in this system; someone on your team
enters what a vendor quoted, by phone, email, or however it arrived.

## When to use it

Record one of these each time a vendor gives you a price for a
requested line — one row per (line, vendor) combination, entered
manually as responses come in.

## Recording a quote

1. Go to **Vendor Quote Line** and choose **New** (or add
   one from wherever your process tracks incoming responses).
2. Choose the **RFQ Line** the vendor is quoting on, and the **Vendor**.
3. Enter the **Unit Price** they quoted.
4. Optionally enter **Quoted At** — when they actually responded, if
   you're tracking that.
5. Save.

## Rules to know

- RFQ Line, Vendor, and Unit Price are all required. Unit Price cannot be
  negative.
- Quoted At is optional — a quote is sometimes entered before you've
  confirmed exactly when the vendor responded.
- One vendor can quote more than one line, and more than one vendor can
  quote the same line — that's the normal shape a comparison needs.

## What it connects to

A Vendor Quote Line references the **RFQ Line** it's quoting and the
**Party** (vendor) who quoted it.
Every quote line for a given RFQ feeds that RFQ's **Compare Quotes**
report.
