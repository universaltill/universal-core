---
title: External SQL Source
audience: admin
module: foundation
order: 23
---

An External SQL Source is a registered connection to an external database
— a legacy ERP's SQL Server, or any Postgres/SQL Server database you want
to pull records from through the import wizard. Unlike **AI Provider
Connection**, this is a list, not a single settings record: you can
register several sources and pick one each time you run an import.

## When to use it

Register an External SQL Source before running a SQL-based import — for
example, migrating customer and item data from a legacy system into this
platform.

## Registering a source

1. Go to **Settings → SQL Sources** and choose to add a new source.
2. Enter a name, the driver (SQL Server or Postgres), host, database, and,
   if needed, port, username, and password.
3. Save. The password is encrypted before it is stored and is never shown
   back to you afterward — leaving the password field blank on a later edit
   keeps the existing password unchanged rather than clearing it.
4. Use **Test** to confirm the connection actually works before relying on
   it for an import.

## Rules to know

- Name, driver, host, and database are required. Username, port, password,
  and extra driver-specific options are optional — some sources
  legitimately need no password (a passwordless local database, for
  example).
- This settings page does not require the `tenant_admin` role specifically
  — unlike AI Provider Connection, any user with access to this settings
  area can manage sources, so keep in mind who has that access.
- Connection details are stored as individual fields, not a single
  connection string, so the settings screen can edit them one at a time and
  the password can be replaced without re-entering everything else.

## What it connects to

**System Of Record** and **External Identity** both reference an External
SQL Source — the former to declare who owns imported records, the latter
to remember which legacy record a given import came from.
