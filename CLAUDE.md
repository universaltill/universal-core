# universal-core — rules for working in this repo

The metadata-driven ERP kernel (Go, Postgres, server-rendered HTMX). This
repo is **public** — governing decision doc and every review record live
in the **private** sibling repo `../uc-infra` instead (moved there
2026-07-19, see `../uc-infra/README.md`): `../uc-infra/docs/adr/0001-
universal-erp-metadata-kernel.md` (self-hosted since 2026-07-18; was
unitill `docs` repo ADR-0017 before Universal Core became a separate
product tree — see the ADR's provenance note) and `../uc-infra/docs/
code-reviews/`. `docs/` and `infra/` are gitignored *in this repo*
specifically so an accidental `git add -A` can never leak either into the
public history. Full standards: `../unitill/docs/reference/coding-
standards.md` (still shared with universal-till). The non-negotiables:

## Data access — repository pattern (same discipline as universal-till)
- **Raw SQL lives only in `internal/data` (repositories) and
  `internal/db/migrations`.** No SQL query text anywhere else.
- Migrations are **append-only** after the first release.

## The kernel/deterministic-core boundary (ADR-0001 §1, §16) — the most
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

## Plugin-first — core stays minimal
The ERP core changes as little as possible. Settings/feature-level behavior
that varies by tenant or jurisdiction is added via a plugin, not a core code
change — a new tenant's country-specific behavior should never require
touching this repo. Country/vertical-specific integrations (ERP connectors,
e-invoicing, statutory reports) are ALWAYS a plugin, following the
`ut-plugin-{type}-{name}` convention (unitill's plugin runtime, reused not
duplicated).
- **Tax calculation is the canonical example**: it differs per country, so
  each tenant's tax rules/rates/jurisdiction logic are a plugin. The kernel
  may host a generic, deterministic posting/ledger interface a tax plugin
  calls into (see `internal/kernel/ledger` above) — but the actual tax
  computation itself never lives in core.
- Default new functionality to a plugin. Only put something in the kernel
  when it's genuinely cross-cutting/deterministic and jurisdiction-
  *invariant* (double-entry ledger posting mechanics, the metadata/workflow
  runtime itself — see the kernel/deterministic-core boundary rule above).
- If a feature needs something the plugin sandbox can't do yet, extend the
  plugin platform (a guarded host function) rather than bypassing it into
  core.

## Testing — near-100% coverage (non-negotiable)
Every change — new feature, bug fix, or refactor — ships with tests at
**every applicable layer**, not just the one that's easiest to write:
- **Unit tests** for the logic itself (entity/form/workflow validation,
  kernel package behavior), including edge cases and error paths, not
  just the happy path.
- **Integration tests** against a real Postgres (`internal/data`,
  seed/publish flows, status-graph seeding, cross-tenant isolation) —
  mocking the database is not a substitute.
- **Smoke tests**: a real compiled binary/server actually starts,
  responds, and serves the routes it claims to.
- **UI/UX and real browser E2E tests** (`internal/e2e`, headless
  Chrome) for anything with a rendered page or client-side behavior — a
  rendered-HTML-string test proves markup structure, never proves CSS is
  actually applied or that inline `<script>` behaves correctly. Assert
  against real computed styles / real DOM interaction, not string
  matches.

**Target near-100% coverage** — as close as practically possible, not
"the happy path plus one edge case." A change without a matching test at
every layer it touches is not done, regardless of how confident manual
verification felt. This applies to existing code too: under-tested code
you touch is debt to close, not a precedent to match. Gate every commit
on the full suite passing (`go build`, `go vet`, `go test ./... -p 1`
against a real local Postgres).

## Audit — AI-actor identity is first-class (ADR-0001 §14)
Every mutation writes an audit row carrying `actor_type` (`human` |
`ai_agent`), `actor_id`, and — when `ai_agent` — `model_version` and an
`input_hash`. This is not optional metadata; write it from the same
transaction as the mutation, via `internal/kernel/audit`, never bolted on
after the fact.

## Multi-tenancy
Every table that isn't global configuration carries `tenant_id`. Every
repository method takes a tenant scope explicitly — no query may rely on
an implicit/ambient tenant context. This is the single most consequential
line of defence against a cross-tenant data leak (see ADR-0001 §3).

## API, formats, i18n
Same conventions as universal-till: responses `{ "data": …, "error": null }`,
JSON **snake_case**, dates ISO-8601, money via a `money.Money`-equivalent
integer-minor-units type. No hardcoded user-facing strings.

Form section titles and the Save action label follow the same
catalog-with-graceful-fallback pattern field labels already use
(`field.{EntityType}.{FieldName}`, `entityDisplayName`'s
`entity.{EntityType}.name`):
- A section's `Title` resolves via `form.{EntityType}.section.{slug}`
  (built by `formrender.SectionCatalogKey`), `TOrDefault`-falling back to
  the literal `Title` declared on the Definition. `slug` lowercases the
  title and collapses every run of characters outside `[a-z0-9]` to a
  single underscore, trimming any leading/trailing one (`"Lead-time
  stages"` → `lead_time_stages`) — never derive this by hand, call
  `formrender.SectionCatalogKey(entityType, title)`.
- The Save button's `Label` resolves via one **global**
  `form.action.save` key (`formrender.SaveActionCatalogKey`), not a
  per-entity one — every production form's Save button uses the
  identical literal text, so there is nothing to key per entity on.
  Other action ops (`workflow.start`/`report.render`/`navigate`) are not
  wired through the catalog yet; none of today's Definitions use one
  with a shared label, so add a per-entity-and-action key the same way
  when a real form needs one, rather than translating hypothetical copy.
- `internal/kernel/formrender/i18n_coverage_test.go` enforces this: it
  walks every module's `AllForms()` and fails if any section or Save
  action is missing a translation in any of the four shipped locales. A
  new module's `forms.go` must add its `form.*.section.*` (and, if it
  has a Save action, confirm `form.action.save` still resolves) keys to
  `internal/i18n/locales/*.json` before this test passes — that's what
  makes this a convention future modules are held to, not just a
  docstring.

## Process
Document-first (ADR-0007): architectural changes get an ADR in
`../uc-infra/docs/adr/` before the code lands. Every substantive change
gets a review doc in `../uc-infra/docs/code-reviews/<date>-<topic>.md` —
**not** in this repo (see the top of this file: this repo is public,
`../uc-infra` is private).
