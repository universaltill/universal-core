---
title: Project
audience: user
module: projects
order: 1
---

A Project is a piece of work with a start date, an optional end date,
and its own budget — the record everything else in this module (Tasks,
logged Time Entries, planned Budget Lines) hangs off of.

## When to use it

Open a Project whenever you need to track a body of work as a unit: its
schedule, who's doing what on it, and what it's budgeted to cost. If the
work is for a customer, link them so anyone looking at the project has
that context immediately.

## Creating a project

1. Go to **Project** and choose **New**.
2. Enter a Project Code (your own reference) and a Name.
3. Optionally choose the **Customer** this project is for.
4. Set a **Start Date**. **End Date** is optional, but if you set one it
   cannot be before the start.
5. Optionally set a **Currency** and a **Budget** — the amount this
   project is planned to cost overall.
6. Save.

Once the project exists, add its **Tasks** and **Budget Lines** directly
on the project form — both are edited in place as part of the project,
not created separately first.

## Rules to know

- Project Code, Name, Start Date, and Status are required. Status
  defaults to **Planned**.
- Budget defaults to 0 and cannot be negative.
- End Date, if set, cannot be before Start Date.
- Status follows a real project lifecycle, not a straight line: Planned
  → Active → Completed is the normal path, but a project can move
  between Active and **On Hold** and back as many times as work actually
  pauses and resumes. **Cancelled** is reachable from Planned, Active, or
  On Hold. Completed and Cancelled are both final — a project that needs
  to restart is a new project, not a reopened one.

## Tasks and budget lines

The **Tasks** and **Budget Lines** sections on the project form let you
add, edit, and remove rows without leaving the project. You can also
open **Task** or **Budget Line** directly and create one from there,
choosing its Project — both routes reach the same record; each is
planning detail that only makes sense attached to its project.

Neither section rolls up into a total shown on the project itself: a
project's estimated-hours total or planned-cost total is the sum of its
own Tasks or Budget Lines at the time you look, not a number stored on
the Project record that could quietly drift out of sync with them.
**Budget** on the project is a separate, single figure you set — what
the project as a whole is committed to, distinct from the category-by-
category planning the Budget Lines section records.

## What it connects to

A Project can reference a **Customer** (any Party). Its **Tasks** and
**Budget Lines** belong to it directly (deleting the project's row does
not cascade-delete them, but they have no meaning shown outside their
project). A Task's own **Logged Time** (Time Entries) belongs to the
task, not the project.
