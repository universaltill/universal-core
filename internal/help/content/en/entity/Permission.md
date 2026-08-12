---
title: Permission
audience: admin
module: foundation
order: 19
---

A Permission grants a **Role** read and/or write access to one record type
— for example, letting the Finance Manager role read and write Vendor
Invoice records. Permissions are additive: a user's actual access is the
union of what every Role they hold grants.

## When to use it

Add a Permission once you have a Role and want to open up (or restrict)
access to a specific record type for anyone holding it.

## Granting access to a record type

1. Go to **Permission** and choose **New**.
2. Choose the Role and enter the record type it applies to (typed exactly
   as it appears elsewhere in the system, e.g. "VendorInvoice").
3. Mark **Can Read** and/or **Can Write** as appropriate.
4. Save.

## Rules to know — this is what actually turns access control on

- **A record type with no Permission row at all stays fully open** to
  every authenticated user — this keeps every record type that existed
  before you set up access control working exactly as before.
- **The moment you create the first Permission row for a record type, that
  record type flips to deny-by-default**: only a user holding a Role with a
  matching Permission row (or `tenant_admin`) can read or write it from
  then on. Creating a narrow grant is also the act of locking that record
  type down — there is no way to add an "allow everyone" Permission
  separately from just leaving it alone.
- There is no "deny" row — you cannot use a Permission to take access away
  from a Role that another Permission (or another Role the same user holds)
  already grants it through.
- **The record type you type here is free text, not a picker** — a
  misspelled name creates a Permission that silently does nothing, since it
  never matches a real record type. Double-check the exact name.
- Creating your very first Permission row (for any record type) also
  switches Role, User Role, Permission, Field Permission, Delegation,
  System Of Record, and External Identity to admin-only editing — see
  **Role**'s own rules for why you should grant yourself `tenant_admin`
  first.

## What it connects to

Every Permission references a **Role**. **Field Permission** works
alongside it for finer, per-field control rather than whole-record-type
access.
