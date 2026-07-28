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

## Authentication

Three ways to authenticate, composed in this order (`internal/api/
handlers.go`'s `Routes`): a connector's Bearer token is checked first,
then real human login, then the insecure dev stopgap.

- **Machine-to-machine (`internal/svcauth`) — what a connector uses.**
  A connector authenticates as its own Zitadel **machine user** (a
  service credential, distinct from any human — see
  `uc-infra/infra/terraform/zitadel/main.tf`'s `zitadel_machine_user`/
  `zitadel_personal_access_token`), presenting a **Personal Access
  Token** as `Authorization: Bearer <token>`. Universal Core validates
  it via Zitadel's token-introspection endpoint (RFC 7662) on every
  request — not local JWT verification — so a revoked credential stops
  working immediately, not just once a self-contained token's own
  expiry passes. The token must carry the `tenant_integration` project
  role (granted to the machine user, never to a human) — a human's
  `tenant_member`-only credential is deliberately rejected by this path.
  Ask whoever operates this deployment for a machine user + PAT scoped
  to your tenant; see `INTEGRATIONS.md`'s "Getting a tenant provisioned"
  section above for the same "ask the operator" pattern.

  An optional `X-On-Behalf-Of: <id>` header lets the connector assert
  which specific human actually initiated a given mutation (see "Every
  write needs a real actor" below) — the service credential vouches for
  this identity; Universal Core trusts the assertion once the
  credential itself is verified, the same trust boundary a till already
  extends to its own logged-in cashier.

- **Real human login** (`internal/webauth`): Zitadel/OIDC, for a person
  logging into the Universal Core UI in a browser and getting a session
  cookie. What a human user at an integration partner uses to look at
  their own tenant directly — not what a backend connector uses.

- **`INSECURE_DEV_AUTH`** (`internal/httpx/devauth.go`): trusts
  `X-Tenant-ID`/`X-Actor-ID` headers verbatim, zero verification. Fails
  **closed** (401) unless explicitly enabled. Dev/test only — never a
  security boundary, never enabled anywhere reachable from outside a
  trusted dev environment, and never what a real connector should use.

## Every write needs a real actor — there is no "system" bucket

Every mutation is written with an `audit.Actor`: `Type` is either
`"human"` or `"ai_agent"` — deliberately **never** a generic third
`"system"`/`"integration"` bucket, because that would hide which of the
two actually authored the change (see `internal/kernel/audit/audit.go`'s
own doc comment — this is a hard rule in this codebase, not a
convention). An AI actor also requires a `model_version`.

**Resolved for machine-to-machine auth**: a connector-authenticated
request is always attributed as a **human** actor — never a new bucket
— because there's always a real, named, accountable party behind it.
By default that's the service credential's own stable identity
(`svc:<zitadel-subject>`) — the right default for a genuinely unattended
call (a nightly catalog sync, no human in the loop). When a connector
knows exactly which person triggered a mutation (Universal Till's
connector creating a record for a completed sale, say), it should send
`X-On-Behalf-Of: <that person's id>` instead — carried through as the
actor id, not a synthetic "till-connector" identity pretending to be a
person. See `internal/svcauth`'s own doc comment for the full reasoning.

## The API surface

All JSON responses use the same envelope:
`{"data": ..., "error": null}` on success, `{"data": null, "error":
"message"}` on failure. Every route below requires auth (see above) —
for a connector, a `Bearer` access token; the tenant is always resolved
from that token (or session), never a query parameter — an entity type
or record only ever resolves within the caller's own tenant.

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
2. **The Zitadel side actually applied.** Machine-to-machine auth
   (`internal/svcauth`) and its Terraform (`uc-infra/infra/terraform/
   zitadel`) are built and tested, but that Terraform hasn't been
   applied against the live, shared Zitadel instance yet — an operator
   needs to run `terraform apply` (creating a real IAM identity + PAT
   needs explicit go-ahead, same as any other secret/identity creation)
   and set `SVC_INTROSPECTION_CLIENT_ID`/`SVC_INTROSPECTION_CLIENT_SECRET`
   on the running deployment before a real connector can authenticate.
3. The connector plugin itself, built on Till's own plugin platform per
   ADR-0014 — that work lives in `../unitill`, not here.

Track progress on all three in `QUEUE.md`.
