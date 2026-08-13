---
title: Task
audience: user
module: projects
order: 2
---

A Task is a unit of work inside a Project — something with a title, an
optional assignee and due date, and a status that tracks its progress.
Tasks can nest under a parent Task, so a large piece of work can be
broken down without leaving the project.

## When to use it

Add a Task for anything worth tracking separately inside a project: a
deliverable, a step, a piece of work you want a due date and an owner
on. Use a parent Task when a task is really a sub-step of a larger one —
the two stay linked, and reporting or filtering can follow the
hierarchy.

## Adding a task

1. On the **Project** form's Tasks section, add a row (or open **Task**
   directly and choose **New**).
2. Enter a **Title**.
3. Optionally choose an **Assignee** — only a Party holding the employee
   role can be picked here.
4. Optionally set **Estimated Hours** and a **Due Date**.
5. If this task is a sub-step of another task on the same project, set
   **Parent Task**.
6. Save.

## Rules to know

- Project, Title, and Status are required. Status defaults to
  **To Do**. Estimated Hours defaults to 0 and cannot be negative.
- **Parent Task** must belong to the same project as the task itself —
  you cannot nest a task under a task from a different project.
- Status is not a straight line: To Do → In Progress → Done is the
  normal path, but **Blocked** is reachable from either To Do or In
  Progress (a task can be blocked before anyone has started it, not only
  once work is underway), and returns to either of those — the graph
  does not track which one it was blocked from. Unlike most document
  workflows in this product, a **Done** task
  can go back to In Progress — reopening the same task when it turns out
  the work wasn't actually finished, rather than losing the time already
  logged against it in a brand-new task. **Cancelled** is reachable from
  To Do, In Progress, or Blocked.

## What it connects to

A Task belongs to a **Project** and, optionally, a **Parent Task** in the
same project. It can have an **Assignee** (Party, employee role only). A
task's **Logged Time** section shows every **Time Entry** recorded
against it — this is a read-only view on the task form; time is entered
from the Time Entry side, not typed directly into the task.
