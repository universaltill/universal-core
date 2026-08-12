---
title: Department
audience: both
module: foundation
order: 8
---

A Department is one node in your organization's structure — Operations,
Warehouse, Finance, and so on. Departments can nest: a Warehouse department
can sit under Operations, building up a real org chart rather than a flat
list.

## When to use it

Set up Departments to represent how your organization is structured, and
use them to scope roles, positions, and (in modules that support it)
approval routing to a specific part of the business.

## Creating a department

1. Go to **Department** and choose **New**.
2. Enter a code and a name.
3. Optionally choose a parent department, to place it under another
   department in the hierarchy.
4. Save.

## Rules to know

- Code and name are required; the parent department is optional — a
  Department with no parent sits at the top level of the org chart.
- Because a Department needs no other setup to exist safely, you can create
  a new one on the fly from the parent-department picker on another
  Department's form, without leaving the page you're on.
- A department has no built-in limit on how deep the hierarchy can go, and
  the system does not check for or prevent a circular reference (a
  department accidentally set as its own ancestor) — keep the structure
  sensible when editing it.

## What it connects to

**Position** records optionally belong to a Department. **User Role**
grants can optionally be scoped to one Department. Once HR is enabled,
employees reference a Department too. Department itself references only
its own parent Department, if any.
