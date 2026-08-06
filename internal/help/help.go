// Package help is the topic-id and content-lookup half of the in-product
// help manual (ADR-0023 in uc-infra, uc-infra#141/#143). It is a small,
// generic support package the generic renderers — internal/kernel/
// formrender for form pages, internal/api's list view for list pages —
// call into to derive a page's help-topic id and check whether that
// topic has content yet, the same relationship internal/i18n already
// has to formrender: neither renderer branches on entity type to use
// this package, and this package never branches on entity type either
// (CLAUDE.md's kernel/deterministic-core boundary rule).
//
// This slice (uc-infra#143) establishes the topic-id convention and the
// disabled-until-content-exists rendering contract only. No manual
// content ships here yet (uc-infra#147-152) and there is no viewer yet
// (uc-infra#144) — see this package's content/README.md and ADR-0023 for
// the full rationale.
package help

import (
	"embed"
	"io/fs"
	"regexp"
	"strings"
)

// contentFS is the embedded content root — see content/README.md for the
// path convention. go:embed for the same self-containment reason
// internal/api/layout.go's htmxJS/appCSS are embedded: an on-prem/
// air-gapped install has no network path to fetch anything at runtime.
//
//go:embed content
var contentFS embed.FS

// TopicID derives the deterministic help-topic id for an entity type's
// generated form/list pages, mirroring formrender.SectionCatalogKey's
// shape: a single exported function is the one place this rule lives,
// so nobody re-derives it by hand. Unlike SectionCatalogKey, entityType
// gets no slugify step — entity.Definition.Validate doesn't constrain
// EntityType's character set today, but every Definition this kernel
// actually ships names it as a clean PascalCase identifier, and it's
// already used unslugified as a URL path segment elsewhere (e.g.
// /forms/{entityType}/new, internal/api/listview.go's NewHref), so this
// package follows that same existing assumption rather than being the
// first place to defend against a case nothing upstream produces or
// validates against.
func TopicID(entityType string) string {
	return "entity/" + entityType
}

// routeSlugRe/slugifyRoute mirror internal/kernel/formrender's
// sectionSlugRe/slugifyTitle byte-for-byte: any run of characters
// outside [a-z0-9] collapses to a single underscore, trimmed at both
// ends. Duplicated rather than shared — this package cannot import
// formrender (the dependency runs the other way: formrender imports
// this package, so the reverse would be an import cycle), and a route
// name (unlike entityType) IS free text a caller might phrase with
// spaces/punctuation, so RouteTopicID needs the same slug treatment
// SectionCatalogKey already gives a Section's Title.
var routeSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugifyRoute(route string) string {
	return strings.Trim(routeSlugRe.ReplaceAllString(strings.ToLower(route), "_"), "_")
}

// RouteTopicID is TopicID's counterpart for a non-entity page (dashboard,
// import wizard, export, reports, admin) — same deterministic-derivation
// shape, keyed by a caller-supplied stable route name instead of an
// entity type. Exported and unit-tested by this slice; wiring it onto
// any specific non-entity page is explicitly deferred to whichever
// future slice or page touches that route next (ADR-0023) — none of
// today's Definitions or handlers call it yet.
func RouteTopicID(route string) string {
	return "route/" + slugifyRoute(route)
}

// Href is the single place the help-viewer URL convention lives —
// reserved now (ADR-0023) so the viewer slice (uc-infra#144) only has to
// implement a handler at this shape, not also invent it, and so every
// "?" affordance this slice renders already points at its final URL.
func Href(topicID string) string {
	return "/help/" + topicID
}

// HasContent reports whether topic-id content exists for locale, falling
// back to English the same direction i18n.Catalog.TOrDefault already
// falls back elsewhere in this kernel: a topic drafted in English but
// not yet translated into a visiting tenant's locale still degrades to
// something once it exists, rather than nothing. Whether every topic is
// translated into all four locales before it's allowed to ship is the
// coverage gate (uc-infra#146), a build-time concern — this is a runtime
// existence check only.
//
// As of this slice landing, this returns false for every topic id: no
// content has shipped yet (see content/README.md), so every generated
// page's "?" renders in its disabled state until uc-infra#147-152 add
// real topics.
func HasContent(locale, topicID string) bool {
	return hasContentIn(contentFS, locale, topicID)
}

// hasContentIn is HasContent's fs.FS-parametrized core, split out so
// help_test.go can exercise both the "found", "falls back to en", and
// "not found anywhere" paths against an in-memory fstest.MapFS without
// needing real embedded content to exist for the test to mean anything.
func hasContentIn(fsys fs.FS, locale, topicID string) bool {
	if topicID == "" {
		return false
	}
	if exists(fsys, "content/"+locale+"/"+topicID+".md") {
		return true
	}
	if locale == "en" {
		return false
	}
	return exists(fsys, "content/en/"+topicID+".md")
}

func exists(fsys fs.FS, path string) bool {
	f, err := fsys.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
