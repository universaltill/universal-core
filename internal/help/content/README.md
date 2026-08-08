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

## Front matter (enforced by the viewer, uc-infra#144)

Every topic file MUST open with a `---`-delimited front-matter block
carrying all four of these keys — the viewer's index builder
(`internal/help/frontmatter.go`) fails the entire build loudly if any is
missing, rather than silently dropping the topic:

```yaml
---
title: Purchase orders
audience: user   # user | admin | both
module: purchasing
order: 1
---
```

- `title` — the topic's display name in the manual's tree and page
  heading. Plain text, not a Definition/field name.
- `audience` — `user`, `admin`, or `both`. This is how the single "one
  manual, admin topics marked" decision (Farshid, uc-infra#141) is
  expressed per topic: it visually badges the topic for admin readers,
  it never hides it from anyone — there is no separate admin-only
  manual.
- `module` — the topic's grouping in the left-pane tree (matches this
  kernel's existing `Definition.Module`/nav grouping convention, e.g.
  `purchasing`, `foundation`).
- `order` — an integer controlling sort position within its module
  group (lower first).

The body below the closing `---` is Markdown, rendered by
`internal/help/markdown.go`'s hand-rolled, deliberately narrow renderer
(headings, paragraphs, `**bold**`/`*italic*`, `` `code` ``, `[text](url)`
links restricted to `http(s)://` or a same-origin `/` path, and `-`/`1.`
lists — no raw HTML). Two style notes that matter because this renderer
is narrower than full CommonMark:
- `_` never starts emphasis mid-word (so `tenant_id`, `actor_type`, and
  this kernel's other snake_case identifiers render as plain text, not
  `<em>`) — write `*emphasis*` if you need italics next to an
  identifier, not `_underscores_`.
- Only `entity/*` and `route/*` topic ids exist today (see the Path
  convention above) — a topic literally named `search` would collide
  with the viewer's own `/help/search` endpoint, so don't add one.
