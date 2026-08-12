---
title: Issue Report
audience: both
module: foundation
order: 7
---

An Issue Report is a bug or feedback report submitted from inside the
product — every tenant gets this, regardless of which modules are licensed,
the same way every tenant has Party. It exists so a user who hits a problem
can tell the team about it without leaving the app.

## When to use it

Use **Report an Issue** (in the navigation bar) any time something looks
wrong, is confusing, or does not work as expected. You do not need to know
which team or module is responsible — just describe what happened.

## Submitting an issue

1. Choose **Report an Issue** from the navigation bar.
2. Type a short title and a description of the problem. You can also record
   a voice note, which is transcribed automatically and appended to your
   description — useful when typing out a long explanation is inconvenient.
3. Submit. The system automatically attaches the page you were on and, if
   your browser logged any errors, its console output — this is what lets
   the team reproduce a problem without you having to describe technical
   details yourself.

## Reviewing issues (admin)

Administrators can open the **Issue Report** list to see everything
submitted, and open an individual report to change its status: **New**,
**Triaged**, or **Dismissed**. This is the same generated form every record
type gets — it is not the submission screen itself, only the triage view.

## Rules to know

- A report always starts in **New** status; there is no required sequence
  after that — an admin can move it directly to Triaged or Dismissed.
- Title, description, and status are required; the voice transcript,
  console log, page, and browser fields are optional and only present when
  the browser actually captured them.
- Every field on a report has a generous but real length limit, so pasting
  an unreasonably large amount of text into a report is rejected rather
  than silently accepted.

## What it connects to

An Issue Report stands on its own — it does not reference other records in
the system. It is captured with the same audit trail as any other record,
so who submitted it and when is always visible in its history.
