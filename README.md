# Universal Core

The metadata-driven ERP kernel: enterprise entities, forms, and workflows
are AI-authored **data**, not per-customer code — reviewed and approved by
a human before they're versioned and published. Sibling product to
Universal Till: Till is the retail/POS edge, Core is the enterprise
backbone it and other systems connect into.

**Status: early but moving fast.** The kernel, a first set of business
modules, and a server-rendered UI exist and are exercised by a full test
suite (unit, Postgres integration, smoke, and browser E2E). Architecture
decisions and review records are kept in an internal decision-record
repo; this README and [`INTEGRATIONS.md`](INTEGRATIONS.md) are the public
entry points.

## What exists today

- `internal/kernel/entity` — the Entity Definition model (fields, types,
  validation, and the three relationship kinds: reference, master-detail
  composition, and related-list).
- `internal/kernel/ledger` — the deterministic double-entry ledger core
  (hand-written, never AI-authored, property-tested for the
  debits-equal-credits invariant).
- `internal/kernel/audit` — AI-actor-aware audit logging: every mutation
  is attributed to a human or an AI agent, with model version and a
  hashed input recorded for the latter, from day one.
- `internal/kernel/crud` — the generic engine: given an Entity Definition,
  provides create/read/update/list against Postgres, with validation and
  an atomic audit entry on every write.
- `internal/kernel/form` — the Form Definition schema: sections, fields
  with conditional `visible_if`, and a closed set of declarative action
  ops. Three distinct section types: plain fields, master-detail
  (composition, with roll-up), and related-list (read-only).
- `internal/kernel/workflow` + `internal/worker` — workflow definitions
  (trigger + a closed set of step kinds), approval halting, and a durable
  Postgres-backed job queue with background dispatchers.
- `internal/kernel/formrender` — the server-rendered HTMX UI, generated
  from Form Definitions.
- Business modules built on the kernel as metadata: finance, purchasing,
  sales, CRM, HR, projects, and assets, plus UBL and SAF-T export.
- `internal/data` — repositories (the only place raw SQL is allowed).
- `internal/db/migrations` — the foundation schema.
- `cmd/universal-core` — the server; `cmd/provision-tenant`,
  `cmd/install-module`, `cmd/seed-demo-data` and friends for operating
  tenants.

## What doesn't exist yet

Connector plugins beyond the first CSV import path, the plugin
runtime/marketplace integration, and module entitlements are designed
but not yet built.

## Integrating with Universal Core

Building a connector or plugin against this system (Universal Till's is
the first)? See [`INTEGRATIONS.md`](INTEGRATIONS.md) for the API surface,
the multi-tenancy/auth model, and what's still missing before a real
unattended connector can be built.

## Running the tests

```
go test ./... -p 1                        # unit tests only, no database needed
TEST_DATABASE_URL=... go test ./... -race -count=1 -p 1   # + Postgres integration
```

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the full pre-push gate
(`-p 1` is required; packages share one test database).

## Contributing

External contributions are welcome — see
[`CONTRIBUTING.md`](CONTRIBUTING.md). All contributors sign a lightweight
CLA ([`CLA.md`](CLA.md)) via a bot comment on their first pull request.

## License

AGPLv3 (see `LICENSE`). The platform is fully open source with no
feature gating; the commercial offering is operated services (managed
cloud hosting, support/SLA), never a license flag in the code.
