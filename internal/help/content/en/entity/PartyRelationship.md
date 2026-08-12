---
title: Party Relationship
audience: both
module: foundation
order: 3
---

A Party Relationship links two **Party** records and states how they relate:
one organization employs a person, one vendor supplies another party, one
organization is the parent of another, or one person is a contact for an
organization. It is the general-purpose mechanism this system uses instead
of a separate, bespoke link for every kind of connection between
organizations and people.

## When to use it

Use a Party Relationship whenever you need to record that two Parties are
connected in a specific way — for example, linking a subsidiary to its
parent company, or recording who the contact person is for a customer
organization.

## Recording a relationship

1. Go to **Party Relationship** and choose **New**.
2. Choose the **From** Party and the **To** Party.
3. Choose the relationship type: **Employs**, **Supplies**, **Parent Of**,
   or **Contact For**.
4. Save.

## Rules to know

- **Direction matters and is not checked for you.** Each relationship type
  reads in one specific direction:
  - *Employs*: the From Party (an organization) employs the To Party (a
    person).
  - *Supplies*: the From Party (a vendor) supplies the To Party (a
    customer).
  - *Parent Of*: the From Party (a parent organization) is the parent of the
    To Party (a subsidiary).
  - *Contact For*: the From Party (a person) is a contact for the To Party
    (an organization) — note this direction is the reverse of *Employs*.
  Getting the From/To order backwards saves without error, so double-check
  which Party belongs in which field before saving.
- A relationship does not require either Party to hold a matching Party
  Role — for example, recording a *Contact For* relationship does not
  itself grant the person a Contact role; add that separately on the Party
  Role screen if it applies.

## What it connects to

Both ends of a Party Relationship are **Party** records. It complements
**Party Role** (which describes one Party's own capacity) rather than
replacing it: a person who is a contact for an organization typically holds
both a Contact Party Role and a *Contact For* Party Relationship pointing at
that organization.
