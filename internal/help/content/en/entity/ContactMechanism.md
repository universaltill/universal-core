---
title: Contact Mechanism
audience: user
module: foundation
order: 5
---

A Contact Mechanism is one way to reach a **Party** — a phone number, mobile
number, email address, or fax number. A Party can have several, which is
why this is its own record instead of a single fixed phone/email pair on
the Party itself: a company with a main line, a sales line, and a fax
number needs all three represented.

## When to use it

Add a Contact Mechanism whenever you have a phone number, email, or fax
number to record for a Party. Add one row per contact channel — a second
phone number is a second Contact Mechanism, not an edit to the first.

## Adding a contact mechanism

1. Open the Party it belongs to, or go to **Contact Mechanism** and choose
   **New**.
2. Choose the Party and the type: **Phone**, **Mobile**, **Email**, or
   **Fax**.
3. Enter the value (the actual number or email address).
4. Mark it **Primary** if it is the one to prefer for that type.
5. Save.

## Rules to know

- Type and value are both required; there is no format validation on the
  value beyond that, so a phone number and an email are both stored as
  plain text.
- **Primary** is a hint, not an exclusive flag — like Address, nothing
  stops more than one Contact Mechanism of the same type being marked
  primary, so keep it to one per type by convention.

## What it connects to

Every Contact Mechanism belongs to exactly one **Party**. It has no other
connections in the system today — it is purely reference information staff
read when they need to reach someone.
