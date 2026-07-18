# Universal Core

The metadata-driven ERP kernel: enterprise entities, forms, and workflows
are AI-authored **data**, not per-customer code — reviewed and approved by
a human before they're versioned and published. Sibling product to
[Universal Till](https://github.com/universaltill/universal-till): Till is
the retail/POS edge, Core is the enterprise backbone it and other systems
connect into.

**Status: early kernel spike**, not a usable product yet. See
[ADR-0017](https://github.com/universaltill/docs/blob/main/adr/0017-universal-erp-metadata-kernel.md)
for the full architecture decision, and `docs/code-reviews/` in this repo
for what's actually been built so far.

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
- `internal/data` — repositories (the only place raw SQL is allowed).
- `internal/db/migrations` — the foundation schema.
- `cmd/universal-core` — a minimal runnable entrypoint (migrations +
  health check); not yet a real API surface.

## What doesn't exist yet

Form Definition rendering (master-detail UI), the workflow/event engine,
the prediction service, connector plugins, module entitlements, and the
base domain models (`erp/reference-data-model.md` in the `unitill` repo)
are all designed in the ADR but not yet built.

## Running the tests

```
go test ./...                    # unit tests only, no database needed
TEST_DATABASE_URL=... go test ./...  # includes Postgres integration tests
```

## License

AGPLv3 (see `LICENSE`) — see ADR-0017 §13 for the reasoning.
