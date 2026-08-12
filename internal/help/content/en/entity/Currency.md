---
title: Currency
audience: both
module: foundation
order: 12
---

A Currency is a currency your business uses — its code, name, and how many
decimal places it uses in practice (its minor unit, e.g. 2 for cents). One
Currency in your tenant is flagged as the base currency: the one everything
else is ultimately measured against.

## When to use it

Set up a Currency for every currency your business trades in, invoices in,
or reports in. Most tenants set this up once, early on.

## Creating a currency

1. Go to **Currency** and choose **New**.
2. Enter the currency code (its standard three-letter code, e.g. "USD" or
   "QAR") and its name.
3. Set the minor unit — how many decimal places it uses (2 for most
   currencies, 0 for currencies with no subunit in practice). It defaults
   to 2.
4. If this is your tenant's base currency, mark it **Base Currency**.
5. Save.

## Rules to know

- Code and name are required. The minor unit must be between 0 and 6.
- Exactly one Currency should be marked **Base Currency**. The system does
  not hard-block a second one, so treat this as an administrative
  convention — features that rely on the base currency (like ledger sync
  and statutory export) fall back safely rather than guessing if it is ever
  ambiguous.
- A Currency record does not carry exchange rates itself — those are
  separate, dated **Exchange Rate** records, since rates change daily while
  a currency's own code and name do not.

## What it connects to

**Exchange Rate** records reference a pair of Currency records. Once
Finance, Sales, or Procurement is enabled, monetary fields on documents
reference a Currency.
