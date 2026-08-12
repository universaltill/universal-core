---
title: Party
audience: both
module: foundation
order: 1
---

A Party is the single record for anyone or anything that can take part in a
business relationship: a person or an organization. It is the one place a
real-world company or individual exists in the system — Core does not keep
separate Customer, Vendor, and Employee tables that could each end up with
their own copy of the same company. Instead, one Party record can hold
several roles at once (see **Party Role**), so a supplier who later starts
buying from you is still one record, not two.

## When to use it

Create a Party whenever you need to record a person or organization the
business deals with — a customer, a vendor, an employee, a prospect, or a
plain contact. You do not create a new Party for each relationship that
person or organization has with you; you create the Party once and then add
a Party Role for each capacity they act in.

## Creating a Party

1. Open **Party** from the Foundation menu and choose **New**.
2. Set the type — **Person** or **Organization** — and enter the name. Both
   are required.
3. Optionally record a tax ID, a preferred language, and (for the
   organization that is your own tenant, see below) a registration number
   and a statutory contact name.
4. Status defaults to **Active**; set it to **Inactive** to retire a Party
   without deleting its history.
5. Save. Then add an **Address**, a **Contact Mechanism**, and one or more
   **Party Role** records to make the Party useful in the rest of the
   system.

## Rules to know

- A Party by itself has no business meaning beyond "this person or
  organization exists" — it becomes a customer, vendor, employee, contact,
  or prospect only once you add a matching Party Role.
- The registration number and statutory contact name fields only matter for
  the one Party that represents your own company (see **Party Role**'s
  **Own Organization** role below) — they are used for statutory exports
  and are otherwise safe to leave blank.
- Exactly one Party in a tenant is expected to be flagged as your own
  organization. The system does not hard-block a second one, so treat this
  as a convention your admin should keep — if it is ever ambiguous, features
  that rely on it (like statutory export) fall back safely rather than
  guessing.

## What it connects to

Addresses, contact mechanisms, and attachments all reference a Party. Party
Role records what capacity a Party acts in; Party Relationship records how
two Parties relate to each other (e.g. one organization employs a person).
Once a module like Purchasing or Sales is enabled, documents such as
purchase orders and invoices reference a Party as the vendor or customer.
