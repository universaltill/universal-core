# universal-core — rules for working in this repo

The metadata-driven ERP kernel (Go, Postgres, server-rendered HTMX). Governing
decision: `docs` repo → `adr/0017-universal-erp-metadata-kernel.md`. Full
standards: `docs` repo → `reference/coding-standards.md` (shared with
universal-till). The non-negotiables:

## Data access — repository pattern (same discipline as universal-till)
- **Raw SQL lives only in `internal/data` (repositories) and
  `internal/db/migrations`.** No SQL query text anywhere else.
- Migrations are **append-only** after the first release.

## The kernel/deterministic-core boundary (ADR-0017 §1, §16) — the most
important rule in this repo
- **Everything under `internal/kernel/entity`, `internal/kernel/form`, and
  `internal/kernel/workflow` is generic and metadata-driven.** It must never
  contain business logic specific to one entity type (no `if entityType ==
  "PurchaseOrder"` inside the generic engine). Entity-specific behaviour
  belongs in an Entity/Form/Workflow *Definition* (data), not in this code.
- **`internal/kernel/ledger` is a deterministic core.** Hand-written,
  human-reviewed, tested (golden-master + property tests for the
  double-entry invariant). Never AI-authored without a human review pass.
  Nothing outside this package posts a journal entry directly.
- **Generated surfaces are never hand-patched.** A fix to a generated CRUD
  screen or API response goes into the Entity/Form Definition or the
  generator, never a one-off patch to generated output.

## Audit — AI-actor identity is first-class (ADR-0017 §14)
Every mutation writes an audit row carrying `actor_type` (`human` |
`ai_agent`), `actor_id`, and — when `ai_agent` — `model_version` and an
`input_hash`. This is not optional metadata; write it from the same
transaction as the mutation, via `internal/kernel/audit`, never bolted on
after the fact.

## Multi-tenancy
Every table that isn't global configuration carries `tenant_id`. Every
repository method takes a tenant scope explicitly — no query may rely on
an implicit/ambient tenant context. This is the single most consequential
line of defence against a cross-tenant data leak (see ADR-0017 §3).

## API, formats, i18n
Same conventions as universal-till: responses `{ "data": …, "error": null }`,
JSON **snake_case**, dates ISO-8601, money via a `money.Money`-equivalent
integer-minor-units type. No hardcoded user-facing strings.

## Process
Document-first (ADR-0007): architectural changes get an ADR in `docs`
before the code lands. Every substantive change gets a review doc in
`docs/code-reviews/<date>-<topic>.md`.
