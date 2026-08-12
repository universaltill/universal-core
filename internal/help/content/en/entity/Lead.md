---
title: Lead
audience: user
module: crm
order: 2
---

A Lead is an unqualified prospect — someone who has shown interest but
is not yet a customer, vendor, or any other kind of Party in the
system. A trade-show sign-up or a web enquiry starts here rather than
as a Party record, because most enquiries never turn into anything, and
creating a full Party record for every one of them would clutter the
party master with names you'll never deal with again.

## When to use it

Log a Lead the moment someone shows interest — a web form submission, a
referral, a trade-show conversation, a cold call. Work it through
qualification, and only convert it once it's real.

## Creating and qualifying a lead

1. Go to **Lead** and choose **New**.
2. Enter the Name and, if known, Company, Email, and Phone — these are
   free text, since a Lead is deliberately not yet a Party record.
3. Choose the **Source** — how the lead arrived (Web, Referral, Event,
   Campaign, Cold Call, Partner, Other) — and, if it names a specific
   **Campaign**, link it.
4. Optionally set an **Owner** (the Party working the lead).
5. Save.

As you work the lead, move its Status along: **New → Contacted →
Qualified**. When it's ready to become a real customer, someone
converts it by hand — creating a Party (holding the **Contact** role
for a person, joined to the organization's Party by a **Contact For**
relationship — see **Party Role** and **Party Relationship**) and
recording that new Party on the Lead's **Converted To** field, then
moving Status to **Converted**.

## Rules to know

- Name and Source are required. Status defaults to **New**.
- Status is a one-way funnel with two exits: **Converted** and
  **Disqualified**, both terminal — a disqualified lead that comes back
  later is logged as a brand-new Lead, not reopened, since a returning
  prospect is a genuinely new opportunity to win, not a continuation of
  the old one. **Disqualified** is reachable from New, Contacted, or
  Qualified — a lead can turn out to be a dead end at any point.
  **Converted** is reachable only from Qualified: a lead has to move
  through the full funnel before it can convert, it cannot jump straight
  from New or Contacted.
- **Converted To** is set by hand — creating the Lead does not
  automatically create anything else, and nothing enforces that the
  Party it points at was actually built the Contact way described
  above; converting a lead correctly is a manual, two-step action
  (create the Party and its role/relationship, then record it here),
  not a button.
- Source and Campaign are independent fields — picking Source =
  Campaign doesn't require a linked Campaign, and linking a Campaign
  doesn't require Source = Campaign. Set both to match reality even
  though nothing checks that they agree.

## What it connects to

A Lead can link to a **Campaign**, and appears in that Campaign's own
Leads list once it does. Once converted, its **Converted To** field
points at the resulting **Party**. An
**Opportunity** can name the Lead it came from in its own **Originating
Lead** field — the link is recorded on the Opportunity, not the other
way around, so one Lead converting into more than one deal over time is
representable.
