---
title: SAF-T Financial Export
audience: admin
module: export
order: 1
---

This page downloads the tenant's general ledger as a SAF-T Financial
audit file (Norwegian SAF-T v1.30) for a date range you choose — a
statutory format some jurisdictions require you to be able to produce on
request.

## When to use it

Use it whenever a tax authority or auditor asks for the general ledger
in SAF-T format for a given period. It reads directly from the ledger —
there's nothing to prepare beforehand beyond having posted the entries
that belong in the file.

## Downloading a file

1. Open **SAF-T export** from the Finance module menu, or go directly
   to `/export/saft/form`.
2. The **From** and **To** dates default to the start of the current
   calendar year and today — adjust them to the period you need. Both
   are required, and **From** cannot be after **To**.
3. Select **Download SAF-T file** to download the XML file for that
   range.

## Rules to know

- The whole file is assembled before the download starts, so a problem
  building it (a bad date range, a permission gate below) is reported as
  a real error, never a truncated file.
- **This is refused outright, not silently redacted, if you cannot see
  everything the file discloses.** It requires read access to Accounts,
  Tax Codes, Parties, and Party Roles, and — for the fields the file
  actually reads (a Party's name, tax id, registration number, and
  contact name; a Tax Code's own fields) — that none of them is hidden
  from you by a Field Permission. A schema-valid file with a silently
  blank statutory field would be worse than no file at all, so this page
  (and the download itself) refuses rather than producing one.
- If the tenant's own organization is identified (a Party holding the
  `own_organization` role, unambiguously), the file's company profile is
  populated from it; otherwise the company fields fall back to "NA" per
  the SAF-T standard, rather than being left empty.
- Every export writes an audit row — who ran it, for what date range,
  and how many ledger entries it contained — whether or not the download
  actually completes.

## What it connects to

Reads the ledger's own **Journal Entry**/GL Account data directly, plus
**Party**/**Party Role** for the trading-partner master files and the
tenant's own company profile, and **Tax Code** for the tax table. This
is a different statutory format from the per-document **UBL export**
available on individual Purchase Orders, Sales Orders, and Customer
Invoices — SAF-T is one whole-ledger file for a period, UBL is one
document at a time.
