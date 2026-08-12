---
title: Case
audience: user
module: crm
order: 4
---

A Case is a support or after-sales issue raised for a customer — a
question, a complaint, or a problem with something they bought. It's the
one place to log a problem and track it through to resolution, whether
or not it concerns a specific product or order.

## When to use it

Open a Case whenever a customer reports a problem or asks a question
that needs tracking to a resolution — not just an email back and forth.
If the issue concerns something you sold them, link the Sales Order and,
if you know which one, the Product, so anyone who picks up the case has
the full context immediately.

## Opening a case

1. Go to **Case** and choose **New**.
2. Enter a Case Number (your own reference) and a Subject.
3. Choose the **Customer** — any Party can be picked here (see Rules
   below).
4. Optionally link the **Product** and the **Sales Order** this case
   concerns.
5. Set the **Priority** — Low, Normal, High, or Urgent — and the
   **Opened** date.
6. Optionally set an **SLA Due** date you're committing to.
7. Save.

## Rules to know

- Case Number, Subject, Customer, Opened, and Priority are all
  required. Status defaults to **New**. Priority is pre-filled with
  **Normal** on the form, but it is still a required field — a case
  created another way (import, API) with no priority set is rejected,
  not silently defaulted.
- Status follows a support workflow, not a straight line: New → In
  Progress → Resolved → Closed is the normal path, but a case can move
  to **Waiting on Customer** from In Progress and back again, and a
  **Resolved** case can return to In Progress if the customer says the
  problem isn't actually fixed — reopening it rather than losing its
  history in a new case. **Cancelled** is reachable from every open
  state, including from Resolved, for a case raised in error or a
  duplicate.
- Unlike a Sales Order's customer field, the Customer here is not
  checked to hold the customer role — any Party can be picked, since
  support is not only for people who have bought from you.
- SLA Due, if you set one, must not be before Opened.
- Product and Sales Order are optional and independent of each other —
  a case can concern neither, either, or both.

## What it connects to

A Case can reference a **Sales Order** and a **Product** (an Item) for
context, and an **Assignee** — the Party actually handling it, which
does not need to be an employment record, since support is often
handled by contractors and partners. The **Customer** is the Party the
case was raised for.
