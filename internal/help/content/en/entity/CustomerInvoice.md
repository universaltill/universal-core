---
title: Customer Invoice
audience: user
module: sales
order: 3
---

A Customer Invoice is a bill sent to a customer against a Sales Order. A
Sales Order records what was agreed; a Customer Invoice is what actually
asks to be paid for it — and, once issued, it is the one that has real
accounting consequences.

## When to use it

Create a Customer Invoice once you're ready to bill the customer for a
Sales Order — typically after the order has shipped or the work is done,
though nothing in the system forces that ordering. It starts in Draft,
where it has no financial effect yet, exactly like an estimate.

## Creating and issuing an invoice

1. Go to **Customer Invoice** and choose **New**.
2. Enter an Invoice Number, and choose the **Sales Order** it bills
   against — the Customer is normally the same one as that order's,
   though nothing auto-fills it for you.
3. Enter the Invoice Date and, if needed, Currency.
4. Enter the Total.
5. Save — this creates the invoice in **Draft**. Nothing has posted to
   the ledger yet.
6. When you're ready to send it, move Status to **Issued** and save
   again. This is the step that has a real bookkeeping effect (see
   below).

## What issuing an invoice actually does

The moment an invoice's status moves to Issued, the system posts a
journal entry for you: **debit Accounts Receivable, credit Sales
Revenue**, both for the invoice's Total. In plain terms — the business
is now owed that money (an asset, Accounts Receivable, goes up) and has
earned that much revenue (income goes up), the two sides of the same
event. This only happens once per invoice: re-saving an already-issued
invoice does not post a second time.

A Draft invoice posts nothing — it's not a financial commitment yet, only
a working copy.

**What moving Status to Paid does *not* yet do**: recording an invoice as
Paid does not currently post a second journal entry (the cash-received
side — debit Cash, credit Accounts Receivable — is real future work, not
built yet). Track actual cash receipt outside the system for now if you
rely on that entry existing.

## Rules to know

- Status starts at **Draft**; the normal path is Draft → Issued → Paid.
  **Void** is reachable from Draft or Issued, but not from Paid — once
  money has actually changed hands, undoing that is a credit note or
  refund, not a status edit.
- Total cannot be negative.
- If the invoice's date falls inside a **Period** whose status is Closed
  or Locked, issuing it is rejected outright — see Period's own topic for
  why.
- Sales Order and Customer are both required.
- An invoice with a Total of zero posts nothing when issued — there is no
  entry to make, and nothing warns you that it was skipped.
- The journal entry is always posted in your organization's base
  currency, using the Total exactly as entered — no exchange-rate
  conversion is applied, even if you set a different Currency on the
  invoice. If you invoice in a currency other than your base currency,
  the amount posted to the ledger will not match the invoice's own total
  in its own currency.

## What it connects to

Every Customer Invoice references the **Sales Order** it bills against
and the **Customer** it bills. Unlike a Sales Order's own Customer field,
this one does not check that the Party actually holds the customer
role — any Party can be picked here, so choose carefully. Issuing one
creates a Journal Entry against the **Account** codes your tenant's
chart of accounts uses for Accounts Receivable and Sales Revenue — if
your chart doesn't use the default codes, issuing will fail to find
them; see Account's own topic (including a real limitation on how
promptly a newly-added Account becomes usable). An invoice can be
exported as a UBL invoice document via the form's "Download UBL file"
action, in any status.
