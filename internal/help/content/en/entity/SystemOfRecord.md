---
title: System Of Record
audience: admin
module: foundation
order: 22
---

A System Of Record declares who the master is for a given record type —
this platform, or an external system you've registered as an **External
SQL Source**. It's how you tell the system "Items are mastered in our
legacy system; don't let anyone hand-edit them here," or the opposite:
"this record type is fully ours now, edit freely."

## When to use it

Set this up when you are importing data from an external system (via the
CSV or SQL import wizard) and need to decide whether records that came from
that system should stay editable here, or should be treated as read-only
mirrors of the legacy system.

## Declaring ownership

1. Go to **System Of Record** and choose **New**.
2. Enter the record type it applies to.
3. If declaring a read-only mirror, choose the External SQL Source it comes
   from.
4. Choose the mode: **Read Only** (the external system is the master; hand
   edits here are blocked for records that came from it) or **Platform
   Owned** (this platform is the master; edit freely). A third mode,
   **Bidirectional**, is reserved for future two-way sync and is rejected
   if you try to save it today.
5. Save.

## Rules to know

- The same record type can legitimately have more than one Read Only
  declaration — for example, if you mirror Party from two different legacy
  systems — as long as each names a different External SQL Source. What is
  actually blocked is declaring the exact same record type and source
  combination twice — that is rejected with an error saying the
  combination is already in use.
- If more than one declaration ever applies to the same record, the most
  restrictive one wins — a record that any applicable declaration marks
  read-only stays protected, rather than the system guessing which
  declaration to trust.
- This is a control-plane record type: once your tenant's access-control
  bootstrap lock has activated, only someone holding `tenant_admin` can
  create or edit System Of Record rows — see **Role**'s own rules for
  exactly what activates that lock and the bootstrap sequencing it
  implies.

## What it connects to

A System Of Record optionally references an **External SQL Source**. It
governs whether records of a given type, once imported, can be hand-edited
through the normal generated forms.
