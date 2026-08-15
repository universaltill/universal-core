---
title: UBL Document Export
audience: user
module: export
order: 2
---

UBL export downloads a single business document — a Purchase Order,
Sales Order, or Customer Invoice — as an OASIS UBL 2.1 XML file, the
open, jurisdiction-neutral vocabulary this system's own data model is
already designed against.

## When to use it

Use it whenever a trading partner or another system needs one document
in a standard, machine-readable format rather than a PDF or a screen —
for example, feeding a Purchase Order or Sales Order into a
Peppol-style e-invoicing pipeline, or archiving a Customer Invoice in a
structured format.

## Downloading a document

There is no standalone Import/Export page for this — open the document
itself and choose **Download UBL file**:

1. Open an existing **Purchase Order**, **Sales Order**, or **Customer
   Invoice**. The action is only available once the record is saved —
   there's nothing to export for a document that hasn't been created
   yet.
2. Select **Download UBL file**. This works in any status; nothing about
   the document's workflow state affects whether it can be exported.
3. The file downloads as `{document number}.xml`.

## Rules to know

- **A Purchase Order or Sales Order becomes a UBL Order document** (UBL
  has one Order type; only which party is the buyer and which is the
  seller changes between the two). **A Customer Invoice becomes a UBL
  Invoice document.**
- A Customer Invoice's UBL file carries exactly **one summary invoice
  line** for its total — this system doesn't yet store a Customer
  Invoice's own line-level detail, and deriving lines from the linked
  Sales Order instead would overstate a partial invoice. A Purchase
  Order/Sales Order's UBL file does carry its real, individual lines.
- **This is refused outright, not silently redacted, if you cannot see
  everything the file discloses** — the same whole-document gate the
  SAF-T export uses: read access to every entity type the document
  touches (the document itself, its lines and items for an order, the
  counterparty Party/Party Role, the Currency), and no Field Permission
  hiding any field the file actually reads. A schema-valid file that
  silently rendered a hidden amount or party name as blank/zero would be
  worse than refusing outright.
- The exporting tenant's own side of the document (name, tax id) is
  resolved the same way the SAF-T export's company profile is — the tax
  id only appears if the tenant's own organization is identified via a
  Party holding the `own_organization` role; otherwise that field is
  simply omitted (UBL, unlike SAF-T, treats it as optional).
- Every export writes an audit row recording which document was
  exported.

## What it connects to

Reads the exported document itself, its lines and Items (for an order),
the counterparty **Party**/**Party Role**, and **Currency** — plus, for
a Customer Invoice, the **Sales Order** it bills against, for the order
number the invoice's UBL file references. This is a different statutory
format from the whole-ledger **SAF-T export** available from the Finance
module — UBL is one document at a time, SAF-T is the whole ledger for a
period.
