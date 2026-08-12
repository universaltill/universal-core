---
title: Field Permission
audience: admin
module: foundation
order: 20
---

A Field Permission hides one specific field, on one record type, from
holders of one **Role** — for example, hiding the tax ID field on Party
from a junior role, while everyone with any access to Party can still see
its other fields. It is deliberately independent of **Permission**: you can
hide a field on a record type that is otherwise fully open to everyone.

A field hidden this way is completely absent from the generated edit
form for that Role — not just disabled or blanked out — while every
other field on the same form still appears normally.

## When to use it

Use a Field Permission when a whole record type should stay visible to a
Role, but one particular field on it is sensitive enough to hide from that
Role specifically — a credit limit, a bank detail, anything you don't want
a given Role to see even though they can otherwise work with that record
type.

## Hiding a field

1. Go to **Field Permission** and choose **New**.
2. Choose the Role, the record type, and the exact field name.
3. Mark **Hidden**.
4. Save.

## Rules to know

- A field is only hidden from a user when **every** Role they hold marks it
  hidden — if a user holds a second Role that does not hide the field, they
  still see it. Field Permission narrows visibility per role; it does not
  override a broader grant from another role the same user holds.
- The field name is free text, matched exactly against the record type's
  real field names — a typo silently does nothing, the same risk as
  Permission's record-type field.
- Hiding a field removes it from the generated form entirely; it is not a
  read-only display, and it is not something layout or styling controls —
  hiding happens at the access-control layer itself.
- Creating your very first Field Permission row (like Permission) switches
  Role, User Role, Permission, Field Permission, Delegation, System Of
  Record, and External Identity to admin-only editing — see **Role**'s own
  rules for why you should grant yourself `tenant_admin` first.

## What it connects to

Every Field Permission references a **Role**. It works alongside
**Permission**, but the two are independent — a record type can have field
hiding configured without any entity-level Permission rows existing at all.
