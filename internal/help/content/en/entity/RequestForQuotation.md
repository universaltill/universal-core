---
title: Request for Quotation
audience: user
module: purchasing
order: 7
---

A Request for Quotation (RFQ) is a request sent to one or more vendors
for pricing on a set of items, before you commit to a Purchase Order. Use
it when you're comparing vendors, not when you've already decided who
you're buying from.

## When to use it

Create an RFQ while you're still gathering pricing — once you've picked a
vendor and price, create a **Purchase Order** instead. Nothing in the
system converts an RFQ into a Purchase Order for you; comparing quotes
here is the last step this record does.

## Creating and sending an RFQ

1. Go to **Request for Quotation** and choose **New**.
2. Enter an **RFQ Number** and a **Due Date**.
3. Save once to create the record, then add rows in the **Vendors**
   section — one per vendor you're inviting to quote — and in the
   **Lines** section — one per item you want priced, with a quantity.
4. Once you've actually sent it to your invited vendors, move Status to
   **Sent** and save. This is a record of what happened, not something
   the system does for you — sending itself happens outside this record.
5. As responses come in, record each vendor's price for each line as a
   **Vendor Quote Line** (see that topic). Once you've
   started recording responses, move Status to **Quotes Received**.
6. Use **Compare Quotes** (an action on this form) to see every vendor's
   price side by side.
7. Once you've decided — picked a vendor, or abandoned the RFQ with an
   answer in hand — move Status to **Closed**.

## Comparing quotes

The **Compare Quotes** action opens a read-only report: one row per
requested line, one column per invited vendor, each vendor's quoted price
where they responded (a genuine blank where they didn't — never a
fabricated zero), with the lowest price in each row marked, and a footer
totalling each vendor's own quoted total across the lines they actually
quoted. This report doesn't do anything but inform your decision — there
is no "select winning quote" action here, and closing an RFQ does not
create a Purchase Order.

## Rules to know

- RFQ Number, Due Date, and Status are all required.
- The normal path is Draft → Sent → Quotes Received → Closed. Cancelling
  is possible from Draft, Sent, or Quotes Received, but not once Closed —
  at that point the comparison is finished, and reopening it is a new
  RFQ.
- Moving through Sent and Quotes Received is manual — nothing about
  adding a vendor, a line, or a quote line automatically advances the
  Status.

## What it connects to

A Request for Quotation has **Vendor** rows (who was invited) and
**Line** rows (what's being priced) in its own sections. Each invited
vendor's response to each line is a separate **Vendor Quote Line**
record. None of these create a **Purchase Order** by
themselves — that's a separate, manual step once you've decided.
