---
title: Role
audience: admin
module: foundation
order: 17
---

A Role is an access-control role you define for your tenant — "Warehouse
Supervisor," "Finance Manager," or anything else your organization actually
needs. Roles are not a fixed system list; every tenant creates exactly the
roles it wants, as many or as few as it needs.

This is an administrative topic: setting up Roles, Permissions, and related
access-control records is normally done by a tenant administrator, not by
day-to-day users.

## When to use it

Create a Role whenever you want to group a set of permissions under a name
you can then grant to one or more users. Roles exist independently of any
specific user — you set up the role first, then grant it via **User Role**.

## Creating a role

1. Go to **Role** and choose **New**.
2. Enter a code (a short, stable identifier) and a display name. A
   description is optional.
3. Save. On its own, a new Role grants nothing — add **Permission** and
   **Field Permission** rows to define what it can actually do, then grant
   it to users with **User Role**.

## Rules to know — read this before creating your first Permission

- **One Role code is special: `tenant_admin`.** A user granted a Role whose
  code is exactly `tenant_admin` always has full access, regardless of any
  Permission or Field Permission rows — this exists specifically so an
  administrator can never lock themselves out.
- **Two separate things each activate the same lock: your tenant getting
  its first Permission or Field Permission row, or anyone at all being
  granted the `tenant_admin` Role.** The moment either has happened,
  several access-control record types — Role, User Role, Permission, Field
  Permission, Delegation, and System Of Record — stop being editable by
  ordinary users and become editable only by someone holding
  `tenant_admin`. Before either has happened, these are open to any
  authenticated tenant member, which is what lets the very first admin get
  set up at all. **Grant yourself (or someone) the `tenant_admin` role
  before you create your first Permission or Field Permission row for
  anything else** — granting the role is itself one of the two things that
  activates the lock, so doing that first never locks anyone out; creating
  a Permission or Field Permission row first, before anyone holds
  `tenant_admin`, would.
- A Role with no Permission rows at all does not restrict anything by
  itself — see **Permission**'s own rules for how entity-level access
  actually gets locked down.

## What it connects to

**Permission** and **Field Permission** rows grant a Role entity- and
field-level access. **User Role** grants a Role to a specific user.
**Delegation** is separate — it lets one user stand in for another's
approval standing, and does not itself use Role at all.
