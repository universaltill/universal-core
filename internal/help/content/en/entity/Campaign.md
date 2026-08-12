---
title: Campaign
audience: user
module: crm
order: 1
---

A Campaign is a marketing activity you can attribute Leads to — an email
blast, a trade show, a paid social push, or any other organized effort
to generate interest.

## When to use it

Create a Campaign before you run the activity, so Leads that arrive
because of it can be linked back to it from the start. Use it to track
what an activity cost and which Leads it produced.

## Running a campaign

1. Go to **Campaign** and choose **New**.
2. Enter a Name and choose the **Channel** — Email, Event, Web, Social,
   Print, Phone, or Partner.
3. Enter **Starts** and, if the campaign has a planned end, **Ends**.
4. Optionally enter a **Budget** and **Currency**.
5. Save.

The **Leads** section on a saved Campaign lists every Lead whose own
**Campaign** field links to it — a read-only view, since a Lead belongs
to the pipeline rather than to the campaign that happened to produce it.

## Rules to know

- Name, Channel, and Starts are required. Status defaults to
  **Planned**.
- Status is a real lifecycle, not something derived from the dates:
  Planned → Active → Completed is the normal path, and a campaign can
  be **Cancelled** from Planned or Active — a campaign cancelled before
  it ran and one that ran its full course are different facts, and only
  the status records which happened.
- Ends, if set, must not be before Starts. Budget cannot be negative.
- A Lead can link to a Campaign in its own **Campaign** field regardless
  of what its **Source** field says — nothing checks that the two agree.
  A Lead can point at a Campaign while its Source is recorded as, say,
  Referral, and it still appears in that Campaign's Leads list. Treat
  the Campaign link as "attributed to," not a strict cross-check against
  Source.

## What it connects to

A Campaign's **Leads** are Lead records whose own Campaign field links
to it. A Campaign does not connect to Opportunity directly — a Lead
that converts carries no automatic trace of the Campaign forward onto
the resulting Opportunity.
