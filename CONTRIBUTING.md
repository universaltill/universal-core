# Contributing to Universal Core

Thanks for considering a contribution. Universal Core is AGPLv3-licensed
open source (see [`LICENSE`](LICENSE)); external contributions are
welcome under the terms below.

## Contributor License Agreement (CLA)

Every contributor signs the project's individual CLA
([`CLA.md`](CLA.md)); pull requests are only merged once the `cla` check
is green. Signing is electronic and takes a few seconds: when you open
your first pull request, the CLA bot comments with a signing phrase —
reply with that phrase (as posted, in its own comment) and the check
turns green for that and all future PRs. Comment `recheck` to re-run the
check if needed.

The CLA does not take away your copyright; it grants the project the
licenses it needs to distribute your work and to keep its licensing
options open long-term. Corporate contributions (work owned by your
employer) are handled case-by-case — open an issue first so we can
arrange an appropriate agreement.

## What to contribute

- **Bug reports and feature requests** — open a GitHub issue on this
  repo. Include reproduction steps for bugs.
- **Bug fixes and improvements** — pull requests against `main`.
- **Connectors, tax logic, country/vertical-specific behavior** — these
  are always **plugins**, never core changes (see "Plugin-first" in
  [`CLAUDE.md`](CLAUDE.md)). If the plugin platform is missing a
  capability you need, open an issue describing the capability rather
  than working around it in core.

## Ground rules for code

These are enforced in review; the full detail lives in
[`CLAUDE.md`](CLAUDE.md):

- **Raw SQL only in `internal/data` and `internal/db/migrations`.**
  Migrations are append-only.
- **The generic engines stay generic** — no entity-specific branching
  inside `internal/kernel/entity`, `form`, or `workflow`; entity-specific
  behavior belongs in Definitions (data).
- **`internal/kernel/ledger` is a hand-reviewed deterministic core** —
  changes there get extra scrutiny.
- **Every mutation writes an audit row** in the same transaction, via
  `internal/kernel/audit`.
- **Multi-tenancy is explicit** — repository methods take a tenant scope;
  no ambient tenant context.
- **No hardcoded user-facing strings** — all labels/messages go through
  the i18n catalog, in every supported locale.
- **API conventions**: `{ "data": …, "error": null }` envelopes,
  snake_case JSON, ISO-8601 dates, money as integer minor units.

## Tests are required

A change ships with tests at every layer it touches — unit,
Postgres-backed integration, smoke, and browser E2E where UI is involved.
Near-100% coverage is the standard, not the aspiration. Run the full
suite before pushing (same gates CI runs, see
[`.github/workflows/ci.yml`](.github/workflows/ci.yml)):

```
test -z "$(gofmt -l .)"          # fails if anything needs formatting
go build ./... && go vet ./...
export TEST_DATABASE_URL=postgres://...   # a real, disposable Postgres
export DATABASE_URL="$TEST_DATABASE_URL"
go run ./cmd/migrate              # CI applies migrations before testing
go test ./... -race -count=1 -p 1
```

`-p 1` is required — packages share one test database and running them
in parallel produces a real, intermittent cross-package race. The SAF-T
XSD validation tests need `xmllint` (`libxml2-utils` on Debian/Ubuntu)
installed, or they skip. A PR with a red suite won't be reviewed.

### `cmd/migrate` is not a setup step for the server or other binaries

The snippet above runs `cmd/migrate` with its default `-target=legacy` —
the original shared-database schema most of the integration suite runs
against. That's a separate, non-chained path from `cmd/provision-tenant`,
`cmd/install-module`, and `cmd/universal-core` itself: all three apply
their own control-plane migration set internally (see
[`cmd/migrate/main.go`](cmd/migrate/main.go)'s doc comment) and need
nothing run before them.

If you go on to run any of those three with the same `DATABASE_URL` you
just exported for the snippet above, you'll hit a deliberate guard: "a
database object it creates already exists — this database most likely
already has a different migration set applied to it". That's the legacy
and control-plane schemas colliding, not a bug — point whichever of
`cmd/provision-tenant`/`cmd/install-module`/`cmd/universal-core` you're
running at its own, separate database instead.

## License of contributions

By contributing, you agree that your contributions are licensed under the
project's license (AGPLv3) and covered by the CLA above. The plugin
SDK/contracts, once published as a separate artifact, are planned to be
Apache-2.0 so third-party plugin authors are not required to open-source
their plugins.
