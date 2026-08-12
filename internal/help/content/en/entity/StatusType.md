---
title: Status Type
audience: both
module: foundation
order: 14
---

A Status Type names one record type's lifecycle — for example, "Purchase
Order Status." It is the top of a three-part mechanism (Status Type,
**Status**, **Status Transition**) that lets a record type have a real,
enforced set of states and legal moves between them, instead of a status
field that lets you jump straight from "draft" to "cancelled" with nothing
in between.

## When to use it

You will not usually create a Status Type yourself — most record types that
use this mechanism (such as purchase orders and vendor invoices, once
Purchasing is enabled) already come with their Status Type, Status, and
Status Transition records set up automatically when that module is turned
on for your tenant. You would only add a new Status Type if you or an
integration is introducing a brand-new record type that needs its own
lifecycle.

## Setting one up

1. Go to **Status Type** and choose **New**.
2. Enter the record type it governs and a code and name for the lifecycle
   (for example, code "purchase_order_status", name "Purchase Order
   Status").
3. Save, then add the individual **Status** values and the **Status
   Transition** rows that connect them.

## Rules to know

- On its own, a Status Type does nothing — a record type only gets enforced
  status behavior once it is specifically built to use one, and once its
  Status and Status Transition records actually exist for your tenant. If
  they are missing, creating or updating that kind of record is rejected
  with an error saying the status lifecycle "is not published for this
  tenant."
- The code you choose here is what a record type's own definition names to
  opt into this lifecycle — it must match exactly.

## What it connects to

Each **Status** belongs to one Status Type. Each **Status Transition**
names the Status Type whose graph it is part of.
