package help

import (
	"fmt"
	"html/template"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// contentLocales are the four locales this repo ships (help.go's own
// HasContent doc comment, internal/i18n/locales/*.json) — BuildIndexFrom
// only ever looks for topics under these four, matching the fixed
// locale set every other i18n surface in this kernel assumes.
var contentLocales = []string{"en", "ar", "fa", "tr"}

// Topic is one topic's metadata, independent of locale/content — the
// front matter (frontmatter.go) plus its id/locale.
type Topic struct {
	ID       string // e.g. "entity/Item", "route/reports" — help.TopicID/RouteTopicID's own shape
	Locale   string
	Title    string
	Audience string // "user" | "admin" | "both"
	Module   string
	Order    int
}

// Rendered is a Topic plus its rendered content — what the viewer
// (internal/api/help.go) actually renders (HTML) and what Search
// matches against (PlainText).
type Rendered struct {
	Topic
	HTML      template.HTML
	PlainText string
}

// Index holds every topic found under content/<locale>/**/*.md for all
// four shipped locales, built once from an fs.FS by BuildIndexFrom.
type Index struct {
	// byLocaleID[locale][id] is that locale's own Rendered topic, absent
	// when the locale has no file for id yet.
	byLocaleID map[string]map[string]Rendered
	// enIDs is the canonical topic id set — en is the source of truth
	// for which topics exist at all (help.HasContent's own fallback
	// direction: en is drafted first, other locales catch up).
	enIDs []string
}

// BuildIndexFrom walks fsys for content/<locale>/**/*.md (each of the
// four shipped locales) and parses every file found. A locale with no
// content/<locale> directory at all (true of every locale against the
// real embedded content today — see content/README.md) is simply
// skipped, not an error: only a file that DOES exist but fails to parse
// (frontmatter.go's parseTopicFile) fails the whole build, per
// ADR-0023's "fail loud, not a silent skip" requirement.
func BuildIndexFrom(fsys fs.FS) (*Index, error) {
	idx := &Index{byLocaleID: map[string]map[string]Rendered{}}
	for _, locale := range contentLocales {
		root := "content/" + locale
		if _, err := fs.Stat(fsys, root); err != nil {
			continue
		}
		topics := map[string]Rendered{}
		walkErr := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".md") {
				return nil
			}
			raw, err := fs.ReadFile(fsys, p)
			if err != nil {
				return fmt.Errorf("help: read %s: %w", p, err)
			}
			fm, body, err := parseTopicFile(raw)
			if err != nil {
				return fmt.Errorf("help: %s: %w", p, err)
			}
			id := strings.TrimSuffix(strings.TrimPrefix(p, root+"/"), ".md")
			htmlOut, plainOut := RenderMarkdown(body)
			topics[id] = Rendered{
				Topic: Topic{
					ID:       id,
					Locale:   locale,
					Title:    fm.Title,
					Audience: fm.Audience,
					Module:   fm.Module,
					Order:    fm.Order,
				},
				HTML:      htmlOut,
				PlainText: plainOut,
			}
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
		idx.byLocaleID[locale] = topics
	}

	enIDs := make([]string, 0, len(idx.byLocaleID["en"]))
	for id := range idx.byLocaleID["en"] {
		enIDs = append(enIDs, id)
	}
	sort.Strings(enIDs) // deterministic base order; Topics()/Search() re-sort by Module/Order/Title anyway
	idx.enIDs = enIDs
	return idx, nil
}

// resolve returns id's Rendered topic in locale, falling back to en —
// the same fallback direction help.HasContent already uses elsewhere in
// this package: a topic drafted in English but not yet translated still
// degrades to something rather than nothing.
func (idx *Index) resolve(id, locale string) (Rendered, bool) {
	if _, ok := idx.byLocaleID["en"][id]; !ok {
		return Rendered{}, false
	}
	if r, ok := idx.byLocaleID[locale][id]; ok {
		return r, true
	}
	return idx.byLocaleID["en"][id], true
}

// ContentLocales returns a copy of the fixed four-locale set this
// package's content walk (BuildIndexFrom) and every coverage gate built
// on it check against — a copy, not contentLocales itself, so a caller
// can't mutate the package-level slice through the returned value.
// Exported so internal/help/coverage_test.go (an external _test package,
// help.go's own TopicID doc comment explains why it has to be one) has a
// single place to read this list from instead of a second hardcoded
// literal that could silently drift from this one the day a fifth
// locale ships (uc-infra#146 review finding).
func ContentLocales() []string {
	out := make([]string, len(contentLocales))
	copy(out, contentLocales)
	return out
}

// OwnPlainText returns id's rendered plain-text body for locale
// specifically, with NO fallback to en — unlike Get/resolve, whose
// fallback exists to answer the *reader's* question ("show me
// something") and is exactly the wrong answer for the *coverage gate's*
// question ("is this locale itself actually translated"). A topic
// present only in en and missing in ar/fa/tr is a real documentation gap
// the same way a missing i18n catalog key is one (CLAUDE.md's i18n
// section); resolve's fallback would hide that gap from any check built
// on it, which is why the coverage gate is built on this method instead.
//
// Returns the parsed PlainText, not just a bool for "file exists": a
// front-matter-only stub with an empty body would satisfy an
// existence-only check while documenting nothing (the exact hole
// i18n_coverage_test.go's hasRealTranslation closes one level up, for
// translation strings instead of help topics) — a caller wanting the
// coverage gate's actual question should check the returned text is
// non-blank, not just that ok is true.
func (idx *Index) OwnPlainText(id, locale string) (string, bool) {
	r, ok := idx.byLocaleID[locale][id]
	return r.PlainText, ok
}

// Topics returns one entry per topic id that has an en file, each using
// locale's own file if present else falling back to en, sorted by
// Module, then Order, then Title (the resolved/localized values, so a
// locale's own translated Title drives sort order for that locale, not
// en's).
func (idx *Index) Topics(locale string) []Rendered {
	out := make([]Rendered, 0, len(idx.enIDs))
	for _, id := range idx.enIDs {
		r, ok := idx.resolve(id, locale)
		if !ok {
			continue // unreachable in practice: enIDs is derived from byLocaleID["en"] itself
		}
		out = append(out, r)
	}
	sortTopics(out)
	return out
}

func sortTopics(rs []Rendered) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Module != rs[j].Module {
			return rs[i].Module < rs[j].Module
		}
		if rs[i].Order != rs[j].Order {
			return rs[i].Order < rs[j].Order
		}
		return rs[i].Title < rs[j].Title
	})
}

// Get is a single-topic lookup, same locale-fallback rule as Topics.
func (idx *Index) Get(topicID, locale string) (Rendered, bool) {
	return idx.resolve(topicID, locale)
}

// Search does a case- and diacritic-insensitive substring match over
// Title and PlainText. An empty (or all-whitespace) query returns nil —
// not an error, not a panic (ADR-0023 acceptance criterion) — including
// against a completely empty index. Title matches rank before
// body-only matches; within each rank, results keep Topics' own stable
// (Module, Order, Title) order.
func (idx *Index) Search(locale, query string) []Rendered {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}
	fq := foldForSearch(q)

	all := idx.Topics(locale)
	var titleMatches, bodyMatches []Rendered
	for _, r := range all {
		if strings.Contains(foldForSearch(r.Title), fq) {
			titleMatches = append(titleMatches, r)
			continue
		}
		if strings.Contains(foldForSearch(r.PlainText), fq) {
			bodyMatches = append(bodyMatches, r)
		}
	}
	return append(titleMatches, bodyMatches...)
}

// foldForSearch case- and diacritic-folds s: Unicode NFKD-normalize
// (decomposes a precomposed character like "İ" into its base letter
// plus combining marks), strip every combining mark (Unicode category
// Mn), then lowercase what's left. golang.org/x/text/unicode/norm is
// already an INDIRECT dependency in go.mod (transitively pulled in via
// another module) — promoting it to direct use needed no new module
// fetch, only `go mod tidy` to move it from the indirect to the direct
// require block, so this isn't a new dependency per CLAUDE.md/this
// slice's own constraint.
func foldForSearch(s string) string {
	s = norm.NFKD.String(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// indexOnce lazily builds the production Index from the real embedded
// contentFS exactly once, the first time GetIndex is called — a
// sync.OnceValues package-level singleton, the same "compute once,
// package-level, no injected dependency" shape help.go's own doc
// comment on TopicID/HasContent already establishes for this package
// (content is a compile-time embed.FS, not runtime-configured).
var indexOnce = sync.OnceValues(func() (*Index, error) { return BuildIndexFrom(contentFS) })

// indexOverrideMu/indexOverride back SetIndexForTesting below — a
// swappable test seam so internal/e2e (a real HTTP server running
// in-process, same Go binary) can point the running server at a small
// fixture Index built from fstest.MapFS/os.DirFS(tempdir) content
// without mutating the package-level default outside the test that set
// it.
var (
	indexOverrideMu sync.RWMutex
	indexOverride   *Index
)

// GetIndex returns the production help Index, built once (lazily) from
// the real embedded content — or, when a test has called
// SetIndexForTesting, that test's override instead.
func GetIndex() (*Index, error) {
	indexOverrideMu.RLock()
	override := indexOverride
	indexOverrideMu.RUnlock()
	if override != nil {
		return override, nil
	}
	return indexOnce()
}

// SetIndexForTesting swaps GetIndex's result for idx until the returned
// restore func runs — callers must defer/t.Cleanup(restore) so the
// override never leaks into a later test. Mirrors this repo's general
// "for testing" seam shape (a package-level var behind a setter that
// returns its own undo) rather than threading an Index through every
// call site as an injected dependency, matching how this package's own
// production lookup (indexOnce above) is a plain package-level
// singleton, not a constructor argument.
func SetIndexForTesting(idx *Index) (restore func()) {
	indexOverrideMu.Lock()
	prev := indexOverride
	indexOverride = idx
	indexOverrideMu.Unlock()
	return func() {
		indexOverrideMu.Lock()
		indexOverride = prev
		indexOverrideMu.Unlock()
	}
}
