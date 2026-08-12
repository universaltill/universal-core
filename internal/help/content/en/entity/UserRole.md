---
title: User Role
audience: admin
module: foundation
order: 18
---

A User Role grants one **Role** to one user. A user can hold more than one
Role, and a Role can be granted to more than one user — it is a many-to-many
grant, not a one-per-user assignment.

## When to use it

Grant a User Role once you have set up a Role with the Permissions it
should carry, and you need a specific person to actually have it.

## Granting a role

1. Go to **User Role** and choose **New**.
2. Enter the user's identifier and choose the Role.
3. Save.

## Rules to know

- Both the user identifier and the Role are required. There is no separate
  user directory to pick from in this screen — the identifier is the same
  login identity the rest of the system already uses for that person.
- A Department can optionally scope a grant, but this is not yet shown on
  this form or enforced by access checks — it is reserved for future
  department-based approval routing. Do not rely on it to restrict a
  grant today.
- Granting `tenant_admin` gives full access regardless of any other
  configuration — see **Role**'s own rules on why you should grant this to
  at least one user before creating any Permission or Field Permission row.
- Removing a User Role revokes that grant immediately; it does not delete
  the Role itself or affect any other user holding it.

## What it connects to

Every User Role references a **Role**. It is the only place a Role actually
reaches a real user — Permission and Field Permission rows describe what a
Role can do, but a user only gets that access once a User Role grants them
the Role itself.
