---
title: Party Role
audience: both
module: foundation
order: 2
---

A Party Role records one capacity a **Party** acts in: customer, vendor,
employee, contact, prospect, or your own organization. It is what turns a
plain Party record into something the rest of the system recognizes —
without a Party Role, a Party is just a name with no business function.

## When to use it

Add a Party Role every time a Party starts acting in a new capacity. The
same Party can hold more than one role at the same time — a supplier who
later also buys from you gets a second Party Role added to the same Party
record, not a second Party.

## Adding a role to a Party

1. Open the Party the role belongs to (or go to **Party Role** and choose
   **New**).
2. Pick the Party and choose the role: **Customer**, **Vendor**,
   **Employee**, **Contact**, **Prospect**, or **Own Organization**.
3. Save. A Party can hold the same role only once in practice — there is no
   reason to add "Vendor" twice for one Party — but nothing stops you from
   adding a second role of a different kind.

## Rules to know

- **Own Organization** is a special role: it marks the one Party record that
  represents your own company, consumed by statutory exports for your
  registration number and tax details (set on the Party itself). Only one
  Party in a tenant can hold it — the system rejects a second attempt to add
  it outright, so there is nothing to remember to enforce by convention. To
  move this role to a different Party, remove it from the current Party
  first and save, then add it to the new one.
- Roles are additive, not exclusive: a Party being a vendor does not stop it
  from also being a customer or a contact.
- Removing a role does not delete the underlying Party or its history —
  addresses, contact details, and attachments stay in place.

## What it connects to

Every Party Role points back to one **Party**. **Party Relationship**
records connections *between* two Parties (such as one organization
employing a person) and is a separate concept from Party Role, which only
ever describes one Party's own capacity. Once Purchasing or Sales is
enabled, a Party needs the matching role (Vendor or Customer) before it can
be used on a purchase order or sales order.
