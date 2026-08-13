---
title: Vendor Invoice
audience: user
module: purchasing
order: 6
---

A Vendor Invoice is a bill received from a vendor against a Purchase
Order — the buy-side counterpart to a Customer Invoice. Before it can be
paid, its total is checked against what was actually received.

## When to use it

Create a Vendor Invoice when a bill arrives from a vendor for a Purchase
Order you've already received against, at least in part. It starts in
Draft, with nothing checked yet.

## Creating and matching an invoice

1. Go to **Vendor Invoice** and choose **New**.
2. Enter an **Invoice Number**, and choose the **Purchase Order** it
   bills against.
3. Choose the **Vendor** — this must be the same vendor as the Purchase
   Order's own, or matching will reject it (see below).
4. Enter the **Invoice Date** and, if needed, **Currency**.
5. Enter the **Total**.
6. Save — this creates the invoice in **Draft**.
7. When you're ready to check it against what was received, move Status
   to **Matched** and save.

## What moving to Matched actually does

The moment you save with Status set to Matched, the system checks three
things: that the invoice's Vendor really is the Purchase Order's own
vendor, that the invoice's Currency (when you set one, and the Purchase
Order also has one) agrees with the Purchase Order's own Currency, and
that the invoice's Total agrees with the total value of everything
actually received against that Purchase Order (every Goods Receipt
Line's quantity times its own line's Unit Price) — to the precision that
Currency actually uses (most currencies to the cent; some, like Japanese
Yen, have no fractional unit at all).

If everything agrees, the invoice becomes Matched. **If it doesn't, the
invoice does not stay Draft and does not reject the save — it moves to
Match Exception instead**, and the **Match Exception Reason** field is
filled in with why (wrong vendor, a Currency that doesn't match the
Purchase Order's own, nothing received yet, no lines on the Purchase
Order, or a value that doesn't agree). Match Exception is a normal,
expected stop on the way to being paid, not an error state you need to
avoid.

This check re-runs every time you save an invoice whose Status resolves
to Matched — including editing an already-Matched invoice's Total, which
can push it back into Match Exception if the edit no longer agrees.

## Getting out of Match Exception

Fix whatever the reason describes — correct the Total, correct the
Currency, wait for the missing Goods Receipt to be recorded, or confirm
the Vendor — and save again with Status set to Matched. If it agrees
this time, the reason is cleared and the invoice becomes Matched.

**There is no direct path from Match Exception to Paid.** That's not an
oversight — it's the actual control that stops an unresolved invoice from
being marked paid. Resolve the exception first.

## Rules to know

- Invoice Number, Purchase Order, Vendor, Invoice Date, and Status are
  all required.
- Total cannot be negative.
- The normal path is Draft → Matched → Paid. Void is reachable from
  Draft, Matched, or Match Exception, but not from Paid — once money has
  moved, undoing that is a credit note, not a status edit.
- Matching does not post anything to the ledger by itself — the Dr
  Inventory / Cr Accounts Payable entry already posted when the goods
  were received (see Goods Receipt). Matching is a check, not a second
  posting.
- Moving Status to Paid does not currently post a cash-received journal
  entry — track actual payment outside the system for now if you rely on
  one existing.

## What it connects to

A Vendor Invoice references the **Purchase Order** it bills against and
the **Vendor** who sent it, and is checked against every **Goods Receipt
Line** recorded against that order.
