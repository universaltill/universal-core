---
title: Opportunity
audience: user
module: crm
order: 3
---

An Opportunity is a pipeline deal — a specific, sizeable chance of
business with a customer or prospect, tracked through to a win or a
loss.

## When to use it

Open an Opportunity once there's a real, nameable deal to track — an
amount, an expected close date, and someone actually working it — not
for every enquiry (that's what Lead is for). An Opportunity commonly
starts life as a converted Lead, but doesn't have to.

## Creating an opportunity

1. Go to **Opportunity** and choose **New**.
2. Enter a Name and choose the **Customer** — the prospect or customer
   the deal is with.
3. If this deal came from a Lead, link it as the **Originating Lead**.
4. Enter the **Amount**, **Currency**, and your estimated
   **Probability %** (as a whole percentage, e.g. 25 for 25%).
5. Enter the **Expected Close** date.
6. Optionally set an **Owner**.
7. Save.

Move the **Stage** field forward as the deal progresses.

## Rules to know

- Name, Customer, and Expected Close are required. Stage defaults to
  **Prospecting**.
- Stage follows the pipeline funnel — Prospecting → Qualification →
  Proposal → Negotiation → Won — but it isn't a straight line:
  Negotiation can move back to Proposal (the customer asks for a
  revised quote) and Proposal back to Qualification (the requirements
  change), and **Won** is reachable directly from Proposal as well as
  from Negotiation, since a customer sometimes accepts a proposal
  outright. **Lost** is reachable from every stage before Won,
  including Prospecting. Won and Lost are both terminal.
- Amount and Probability % cannot be negative; Probability % cannot
  exceed 100.
- The Customer does not have to match anything about the linked
  Originating Lead — nothing checks that the Lead's own Company or
  Converted To Party has any relationship to this Opportunity's
  Customer. Set it correctly; nothing will catch a mismatch.

## What it connects to

An Opportunity's **Customer** is a Party — commonly one holding the
Prospect role for a deal still being won, or the Customer role once
they've bought from you before. Its **Originating Lead**, if any, is
the Lead it grew out of.
