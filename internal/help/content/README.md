# Help content

This directory is the content root for `internal/help`'s in-product
manual (ADR-0023, uc-infra#141/#143). It is embedded into the binary via
`//go:embed content` in `internal/help/help.go`, so a topic ships with
the code that documents it and needs no network access at runtime — the
same self-containment reasoning `internal/api/layout.go` already applies
to `htmxJS`/`appCSS`.

This is deliberately **not** `universal-core/docs/` — that path is
gitignored specifically so this repo's ADRs and code-review records
(which live in the private `uc-infra` repo) can never leak into the
public repo's history via an accidental `git add -A`. This directory is
the opposite: it's meant to ship in the public repo, since the manual it
holds documents the public product.

## Path convention

A topic's file lives at `<locale>/<topicID>.md`, where `<locale>` is one
of this repo's four shipped locales (`en`, `ar`, `fa`, `tr`) and
`<topicID>` is whatever `help.TopicID`/`help.RouteTopicID` derived for
the page it documents — see those functions' own doc comments in
`help.go` for the exact derivation rule. For example, the `PurchaseOrder`
entity's English topic lives at `en/entity/PurchaseOrder.md`.

## Front matter (planned, not yet enforced)

Each topic file will carry a YAML front-matter block, at minimum:

```yaml
---
audience: user   # or: admin
---
```

`audience` is how the single "one manual, admin topics marked" decision
(Farshid, uc-infra#141) is expressed per topic, rather than a naming
convention or a second directory tree. Parsing/enforcing this field is
the viewer's job (uc-infra#144) — this slice (uc-infra#143) only
establishes the path convention and the disabled-until-present `?`
affordance; no topic content ships yet, and `help.HasContent` degrades
every current topic id to "not yet available" honestly rather than
linking to a 404.
