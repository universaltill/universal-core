---
title: Attachment
audience: user
module: foundation
order: 6
---

An Attachment is a file reference that can be attached to any record in the
system — a Party, a purchase order, an invoice, or anything else. It exists
as one generic entity rather than a separate "attachments" feature per
record type, so any part of the system that adopts it gets file attachments
for free.

## When to use it

Use an Attachment whenever you need to keep a file — a scanned document, a
contract, a signed form — alongside a record. A common example is attaching
a vendor's tax form to their Party record, or a signed agreement to a
purchase order.

## Adding an attachment

1. Go to **Attachment** and choose **New** (or use the attach action from
   the record it belongs to, where available).
2. Record which record it belongs to (its type and ID), the file name, MIME
   type, size in bytes, and where the file is stored.
3. Save.

## Rules to know

- File size must be zero or greater — a negative size is rejected.
- Who uploaded an attachment is not a field on the Attachment record itself;
  it is captured automatically in the record's audit history the same way
  it is for every other kind of record, so you do not need a separate
  "uploaded by" field to know who added a file.
- An Attachment can point at any record type in the system, current or
  future — it is not limited to a fixed list of entity types.

## What it connects to

An Attachment references the record it belongs to by that record's type and
ID — it can point at a **Party**, an **Issue Report**, or, once other
modules are enabled, documents like purchase orders. Deleting the record an
Attachment belongs to does not automatically delete the Attachment.
