---
title: Exchange Rate
audience: both
module: foundation
order: 13
---

An Exchange Rate is a dated conversion rate between two **Currency**
records — for example, 1 US Dollar equalled 3.64 Qatari Riyals on a given
date. It is kept as its own record, separate from Currency itself, because
rates change often while a currency's code and name do not.

## When to use it

Add an Exchange Rate whenever you need to record (or update) the rate
between two currencies as of a specific date — typically as part of a
regular update from your bank or a market data source.

## Recording a rate

1. Go to **Exchange Rate** and choose **New**.
2. Choose the **From** currency and the **To** currency.
3. Enter the effective date and the rate — the number you multiply an
   amount in the From currency by to get the equivalent amount in the To
   currency.
4. Save.

## Rules to know

- The rate must be zero or greater; a negative rate is rejected. A rate of
  zero is technically accepted even though it collapses every converted
  amount to zero, the same caveat as UoM Conversion's factor — double-check
  the value you enter.
- The From→To direction is a convention, the same as UoM Conversion's
  factor: enter the currency pair consistently (for example, always foreign
  currency to base currency) so anything that reads these rates later
  multiplies the right way.
- Rates are date-effective, not a single current value — you can hold
  several Exchange Rate records for the same currency pair on different
  dates, building a history rather than overwriting the previous rate.

## What it connects to

An Exchange Rate references two **Currency** records. Once Sales or
Procurement handles multi-currency documents, they draw on Exchange Rate
records to convert amounts.
