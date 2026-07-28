# Integrating with Universal Core

This document is for developers building something that talks to
Universal Core from the outside — a connector plugin, a script, another
service. It describes the API surface that exists **today**, is honest
about what doesn't exist yet, and lays out the first integration this is
being written for: **Universal Till**, via a connector plugin on Till's
own plugin platform.

**Status: early.** Universal Core is a young kernel (see `README.md`,
`CLAUDE.md`). The contract below is real and tested, but it will grow —
in particular, the entities a Till connector actually needs to write to
(sales/invoices) don't exist yet (see "What Universal Till's connector
still needs" below). Treat everything here as "correct as of the commit
you're reading it at," not a frozen spec.

## The integration model

Universal Core is metadata-driven: instead of a bespoke API per business
object, every business object is an **Entity Definition** (fields, types,
validation, relationships) plus a **Form Definition** (how it's edited by
a human) — both AI-authored data, reviewed and approved by a human,
versioned, then published. Once an Entity Definition is published, it
gets a real, generic CRUD API for free: `GET/POST /api/records/{entityType}`,
no per-entity handler code. This is the same mechanism every entity in
this system uses, including the ones a real user edits by hand — an
integration partner isn't a second-class API surface bolted on afterward.

**Multi-tenancy is database-per-tenant** (ADR-0003): every tenant —
including whatever tenant an integration partner's data lives under — is
its own physical Postgres database on the same server. There's a small
control-plane database (`tenants` registry) and one database per tenant,
resolved per request via `internal/tenantdb.Router`. This means one
tenant's data can never leak into another's by a missing `WHERE` clause —
it's a different database connection entirely.

## Getting a tenant provisioned

There's no self-serve signup yet. A tenant is provisioned by an operator
running `cmd/provision-tenant`:

```
DATABASE_URL=<control-plane db> go run ./cmd/provision-tenant \
  -name "Acme Corp" -actor-id "farshid" -modules purchasing
```

This creates the tenant's own database, migrates it, and publishes the
**foundation** layer (always on — see "What entities exist today" below)
plus any named modules (`purchasing` is the only one that exists today).
`-tenant-id` reuses an existing tenant instead of creating one — safe to
re-run to pick up a newly added module later (every `Publish` call is
idempotent).

For a real integration partner, "getting a tenant" today means asking
whoever operates this deployment to run that command for you, not calling
an API.

## Authentication — read this before building anything

**There is no machine-to-machine authentication yet.** This is the
single biggest gap for any backend connector (Till's included) and
should be resolved before anything unattended is built against this API.

What exists today:

- **`INSECURE_DEV_AUTH`** (`internal/httpx/devauth.go`): trusts
  `X-Tenant-ID`/`X-Actor-ID` headers verbatim, zero verification. Fails
  **closed** (401) unless explicitly enabled. This is a dev/test stopgap
  only — it is not a security boundary, and it must never be enabled on
  anything reachable from outside a trusted dev environment.
- **Real human login** (`internal/webauth`): Zitadel/OIDC, for a person
  logging into the Universal Core UI in a browser and getting a session
  cookie. This is what a human user at an integration partner would use
  to look at their own Universal Core tenant directly — not what a
  backend service calling the API programmatically would use.

Neither is right for a connector plugin running unattended in the
background (e.g. syncing catalog data on a schedule, or forwarding a
completed sale with no human present to log in). That needs a real
service-account / client-credentials style mechanism against the same
Zitadel instance (`id.universaltill.com`) already used for human login —
**not yet built.** Don't design around headers or cookies for a
production connector; treat this as a blocking prerequisite and check
`QUEUE.md` for its status before starting.

## Every write needs a real actor — there is no "system" bucket

Every mutation is written with an `audit.Actor`: `Type` is either
`"human"` or `"ai_agent"` — deliberately **never** a generic third
`"system"`/`"integration"` bucket, because that would hide which of the
two actually authored the change (see `internal/kernel/audit/audit.go`'s
own doc comment — this is a hard rule in this codebase, not a
convention). An AI actor also requires a `model_version`.

**What this means for a connector:** when Universal Till's connector
creates a record in Universal Core on behalf of a sale, it needs a real
answer to "who is the actor" — most likely the human cashier/operator
who actually completed the sale on the till, carried through as the
actor id, not a synthetic "till-connector" identity pretending to be a
person. If a fully unattended sync (e.g. a nightly catalog pull with no
human in the loop) needs to write something, how that gets attributed
honestly under this two-bucket model is an open question — flag it
rather than inventing a workaround; it may need its own small ADR before
being decided.

## The API surface

All JSON responses use the same envelope:
`{"data": ..., "error": null}` on success, `{"data": null, "error":
"message"}` on failure. Every route below requires auth (see above) via
`X-Tenant-ID`/`X-Actor-ID` today; the tenant is always implicit in the
URL scope, never a query parameter — an entity type or record only ever
resolves within the caller's own tenant.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/records/{entityType}` | List every record of that type |
| POST | `/api/records/{entityType}` | Create a record |
| GET | `/api/records/{entityType}/{id}` | Get one record |
| POST | `/api/records/{entityType}/{id}` | Update a record (optimistic locking, see below) |
| GET | `/export/{entityType}` | CSV export of every record of that type |
| GET | `/import/{entityType}`, `POST .../preview`, `POST .../commit` | Browser-driven CSV/XLSX import wizard — see caveat below |
| GET | `/api/workflow-jobs`, `POST /api/workflow-jobs/{id}/approve` | List/approve workflow jobs waiting on a `require_approval` step |
| GET | `/reports/purchasing` | The purchasing management report (HTML, human-facing) |

A record's wire shape (`recordResponse` in `internal/api/handlers.go`):

```json
{
  "id": "…",
  "entity_type": "Item",
  "data": { "sku": "STEEL-BAR-10", "name": "10mm Steel Rebar" },
  "version": 3
}
```

`version` is optimistic-locking: an update must round-trip it back as a
`_version` field (form-encoded or JSON body, matching whatever
`Content-Type` the create/update request itself used) equal to the
version last read; a mismatch is a `409 Conflict` ("was changed by
someone else — reload and try again"), not a silent overwrite. A
connector doing a read-modify-write cycle must carry `version` through
exactly the same way a human editing the record in a browser does — this
isn't a UI-only nicety.

**The `/import` wizard is not meant for machine-to-machine bulk load.**
It's a two-step, session-shaped flow (upload → preview a suggested
column mapping → commit) built for a human fixing up a CSV in a browser.
A programmatic bulk sync (e.g. catalog data from Till) should create/
update records one at a time via the plain `POST /api/records/{entityType}`
JSON API instead — same as any other API client. `GET /export/{entityType}`
(plain CSV, no wizard) is the right shape for a **pull-based** sync going
the other direction.

## What entities exist today

Published automatically for every tenant (the **foundation** layer,
`internal/kernel/foundation`): `Party`, `PartyRole`, `PartyRelationship`,
`Address`, `ContactMechanism`, `Attachment`, `UnitOfMeasure`,
`UomConversion`, `Currency`, `ExchangeRate`, `Status`, `StatusType`,
`StatusTransition`, `IssueReport`, `AIProviderConnection`.

Published if a tenant opts into the **purchasing** module
(`internal/kernel/purchasing`): `Item`, `PurchaseOrder`, `POLine`,
`InventoryItem`, `GoodsReceipt`, `GoodsReceiptLine`.

That's it — there is no sales/invoicing module yet (see below). A new
integration-specific entity is added the same way every other entity in
this kernel is: author an Entity + Form Definition, get it
reviewed/approved by a human, publish it. It is never a one-off API
endpoint or a hand-patched schema change (`CLAUDE.md`'s generated-
surfaces-are-never-hand-patched rule applies to an integration's
entities exactly as much as anyone else's).

**Contract stability**: once an Entity Definition is published,
migrations in this repo are append-only and a Definition's fields only
ever grow, never get silently repurposed or removed out from under a
caller (same discipline `../unitill`'s ADR-0014 holds its own
`SaleCompletedEvent` contract to). A connector reading `data` fields
should tolerate new, unrecognized keys appearing over time rather than
assuming an exhaustive fixed set.

## Universal Till — the first integration

Universal Till (`../unitill`) already has its own architecture for this,
on the till side: `ut-docs/adr/0014-erp-integration-connectors.md`
defines a reusable **connector plugin** (`type: integration`) that:

- **Outbound**: subscribes to Till's `sale.completed` plugin-bus event
  (a stable, versioned payload — sale id, receipt number, totals, line
  items, payments) and forwards it to the target ERP, queuing/retrying
  in its own plugin storage if the ERP is unreachable (checkout itself
  never waits on this).
- **Inbound**: pulls catalog/price/stock from the ERP on a schedule and
  feeds it through Till's existing catalog-import seam.

A Universal-Core-specific connector plugin would map onto the surface
described above: outbound writes go through `POST /api/records/{entityType}`
against whatever Sales entity ends up representing a completed sale;
inbound catalog/stock sync reads through `GET /api/records/{entityType}`
(or `GET /export/{entityType}` for a bulk pull) against `Item`/
`InventoryItem`.

### What Universal Till's connector still needs, before it can be built for real

1. **A Sales module.** `sale.completed` needs somewhere to land — a
   `SalesOrder`/`SOLine`/`CustomerInvoice`-shaped Entity+Form Definition
   set (see `reference-data-model.md` §5 for the intended shape;
   `purchasing`'s `PurchaseOrder`/`POLine`/`GoodsReceipt` set is the
   closest existing precedent for what this looks like once built).
   Doesn't exist yet — this repo only has the purchase side today.
2. **Machine-to-machine auth**, as above — a connector runs unattended,
   which today's auth options don't support.
3. **A decision on actor attribution for automated writes**, as above.
4. The connector plugin itself, built on Till's own plugin platform per
   ADR-0014 — that work lives in `../unitill`, not here.

Track progress on all four in `QUEUE.md`.
