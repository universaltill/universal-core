---
title: Depreciation Schedule
audience: user
module: assets
order: 2
---

A Depreciation Schedule row is one period of a Fixed Asset's
depreciation — a sequence number, the last day of the period it covers,
how much to depreciate in that period, and the book value left
afterward.

## When to use it

You won't normally add these rows by hand — saving a **Fixed Asset**
generates its whole schedule automatically (see that entity's own help
topic). Come here to review it, or to correct a period that's wrong
before it's posted. A straight-line schedule spreads an asset's
depreciable amount (cost minus salvage value) evenly across its useful
life in months, with each row's book value equal to the previous row's
book value minus that row's depreciation amount, ending at the salvage
value.

## Rules to know

- Fixed Asset, Sequence, Period End, Depreciation Amount, and Book Value
  are all required. Depreciation Amount and Book Value cannot be
  negative.
- Sequence orders the rows (1, 2, 3, …); it isn't itself bounded, since
  it's a display/ordering value rather than an amount.
- Period End is the last day of the month the row covers — the date a
  posting for that row carries.
- **Posted**, once set, marks the row as already posted to the ledger. A
  posted row reflects a real accounting entry — correct it with the same
  care you'd correct any other posted record.

## What it connects to

A Depreciation Schedule row belongs to exactly one **Fixed Asset**.
