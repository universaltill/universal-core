---
title: Approvals
audience: both
module: workflow
order: 1
---

This is your approvals inbox — every workflow job currently waiting on a
`require_approval` step, across every entity type, with an Approve
button for the ones you're eligible to act on.

## When to use it

Check it whenever you need to see what's waiting for your sign-off, or
when someone says a workflow is stuck at approval. It's reached from the
top navigation bar's **Approvals** link, available to any signed-in user
— not scoped to one module, since a workflow can trigger against any
entity type a tenant has.

## Approving something

1. Open **Approvals** from the top navigation.
2. Each row shows the workflow's name, the entity type and record it's
   waiting on (a link to the record itself), and either an **Approve**
   button or a reason you can't use it yet.
3. Select **Approve**. The row disappears once the job resumes; nothing
   else on the page needs a manual refresh.

## Rules to know

- **This inbox only ever shows `waiting_approval` jobs** — it isn't a
  general workflow status browser. A job that's queued, running, failed,
  or already resumed doesn't appear here at all.
- **You only get an Approve button if you actually hold the role the
  step requires** — and, if the step scopes approval to the record's own
  department, only if you hold that role *in that department*. A row you
  can't act on still shows, with the reason (which role, and which
  department if relevant), so a pending approval is never invisible to
  you just because it isn't yours to approve — it's shown, not hidden.
- **A Delegation can let you approve on someone else's behalf.** If
  another user has delegated their approval standing to you (see
  **Delegation**), you can approve a job that requires a role you hold
  through that delegation, exactly as if it were your own standing.
- **A workflow step can optionally set an escalation**: after a
  configured number of hours waiting, a job becomes eligible for a
  second, wider role to approve it too — in addition to, never instead
  of, whoever could already approve it. This is configured by whoever
  authors the workflow, not from this page; nothing here shows you
  whether a given job has escalated yet beyond that role now also being
  offered an Approve button.
- Approving here is exactly what the underlying API does — this page
  offers nothing the API wouldn't also accept, and refuses nothing the
  API would allow.

## What it connects to

Lists **Workflow Job** rows, linking through to whatever record each one
is waiting on. Who can approve a given row depends on the workflow's own
**Workflow Definition** (its `require_approval` step's role and optional
department scope and escalation settings) and, for a substitute
approver, an active **Delegation** — see that topic for how delegation
itself is set up.
