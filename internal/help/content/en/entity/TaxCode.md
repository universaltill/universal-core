---
title: Tax Code
audience: admin
module: finance
order: 4
---

A Tax Code is a named tax rate a document line can point at — "VAT 20%,"
"Withholding 5%," and so on. It is reference data only: this system does
not calculate which tax code applies to a given transaction, or compute
tax amounts for you. Which code applies to which sale or purchase, and
any country-specific tax logic, is deliberately kept out of the core
product — that decision is made elsewhere (by you, or by a
jurisdiction-specific add-on), not by this record.

## When to use it

Set up the Tax Codes your business actually uses once, early on — most
tenants need only a handful (a standard rate, a reduced rate, maybe a
withholding rate).

## Creating a tax code

1. Go to **Tax Code** and choose **New**.
2. Enter a Code and Name (e.g. "VAT20", "Standard VAT").
3. Enter the Rate as a percentage — enter "5" for 5%, not "0.05".
4. Choose the Tax Type: VAT, Withholding, or Sales Tax.
5. Optionally enter a Jurisdiction (a free-text note — country, region,
   whatever helps you tell codes apart).
6. Save.

## Rules to know

- Code, Name, Rate, and Tax Type are required.
- Rate cannot be negative, but there is deliberately no upper limit —
  some jurisdictions have compound or luxury rates above 100%, and this
  field doesn't second-guess that.
- Enter Rate as a whole percentage number (5 for 5%), not a fraction.
- Code is not schema-enforced unique — keep your own numbering
  consistent.

## What it connects to

Tax Code feeds the SAF-T statutory export alongside **Account**. Nothing
in this first release of the product actually assigns a Tax Code to a
Sales Order or Customer Invoice line yet — that wiring is separate,
future work.
