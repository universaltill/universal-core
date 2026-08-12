---
title: Address
audience: user
module: foundation
order: 4
---

An Address is a postal address that belongs to a **Party**. A Party can have
any number of addresses of different types — a billing address in one
country and a shipping address in another is a normal, supported case, not
an exception.

## When to use it

Add an Address any time you need to record where a Party is located for
billing, shipping, or its registered/legal address. A Party with no address
yet is fine — add one whenever it becomes relevant, such as before creating
a purchase order that needs a shipping destination.

## Adding an address

1. Open the Party the address belongs to, or go to **Address** and choose
   **New**.
2. Choose the Party and the address type: **Billing**, **Shipping**,
   **Registered**, or **Other**.
3. Fill in the address lines, city, and country code (a two-letter country
   code). Region and postal code are optional, since not every country uses
   them.
4. Mark the address **Primary** if it is the default one to use for that
   type.
5. Save.

## Rules to know

- Line 1, city, and country code are required; line 2, region, and postal
  code are optional.
- Marking an address **Primary** is a hint for which one to prefer when a
  Party has more than one of the same type — it is not enforced as
  exclusive, so nothing stops two addresses of the same type both being
  marked primary. Keep only one marked per type in practice.
- Deleting an Address does not affect the Party record itself or any other
  address it has.

## What it connects to

Every Address belongs to exactly one **Party**. Documents that need a
shipping or billing location (once Purchasing or Sales is enabled) draw on
a Party's addresses rather than storing address text of their own.
