---
title: AI Provider Connection
audience: admin
module: foundation
order: 25
---

An AI Provider Connection is your tenant's own configured AI backend for
text-generation-assisted features, such as suggested column mapping in the
import wizard. Every tenant already gets a shared, self-hosted default for
free; this is how you opt into your own Anthropic or OpenAI account
instead (your own cost, your own key, your own explicit choice about that
provider's data handling), or into your own separately hosted server rather
than the shared one.

## When to use it

Configure this only if you specifically want to use your own AI provider
account instead of the shared default — most tenants never need to touch
this at all.

## Configuring your connection

1. Go to **Settings → AI Provider**. This page requires the `tenant_admin`
   role specifically — a user holding a different, non-admin role gets an
   access-denied page here, even if that user has other real access
   elsewhere in the system.
2. Choose your provider: the shared self-hosted option, or your own
   Anthropic or OpenAI account.
3. For your own hosted server, enter its address. For Anthropic or OpenAI,
   enter the model and your API key.
4. Save. The API key is encrypted before it is stored and is never shown
   back to you afterward.

## Rules to know

- There is exactly one AI Provider Connection per tenant — this is a
  single settings record, not a list like External SQL Source. Saving
  again updates the same record rather than creating a new one.
- This record type has no generated data-entry form of its own, and cannot
  be created or edited through the ordinary record screens even by an
  admin — Settings → AI Provider is the only way to change it, precisely
  because the API key needs to stay encrypted and never round-trip back to
  a browser as plain text.

## What it connects to

An AI Provider Connection stands on its own — it is consulted by
AI-assisted features (like import mapping suggestions) rather than being
referenced by other records.
