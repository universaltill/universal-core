# Universal Core

The metadata-driven ERP kernel: enterprise entities, forms, and workflows
are AI-authored **data**, not per-customer code — reviewed and approved by
a human before they're versioned and published. Sibling product to
Universal Till: Till is the retail/POS edge, Core is the enterprise
backbone it and other systems connect into.

**Status: early kernel spike, not yet public.** This repo exists only
locally so far — not pushed to GitHub, pending review (see
`docs/code-reviews/2026-07-18-universal-core-kernel-spike.md` for why).
Full architecture decision: `docs/adr/0001-universal-erp-metadata-kernel.md`
(this repo's own ADR-0001; was unitill `docs` repo ADR-0017 before this
became a separate product tree).

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
- `internal/kernel/workflow` — workflow definitions (trigger + a closed
  set of step kinds) and a synchronous executor that halts at the first
  approval step rather than running through automatically.
- `internal/data` — repositories (the only place raw SQL is allowed).
- `internal/db/migrations` — the foundation schema.
- `cmd/universal-core` — a minimal runnable entrypoint (migrations +
  health check); not yet a real API surface.

## What doesn't exist yet

An actual form *renderer* (HTML/HTMX output from a Form Definition), the
durable/transactional Postgres job queue for workflows (retries,
dead-letter, resume — today's executor is synchronous and in-memory), the
prediction service, connector plugins, module entitlements, and the base
domain models (kept in an internal, non-public reference document) are
all designed in the ADR but not yet built.

## Running the tests

```
go test ./...                    # unit tests only, no database needed
TEST_DATABASE_URL=... go test ./...  # includes Postgres integration tests
```

## License

AGPLv3 (see `LICENSE`) — see ADR-0001 §13 for the reasoning.
