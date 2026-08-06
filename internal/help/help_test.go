package help

import (
	"testing"
	"testing/fstest"
)

func TestTopicID(t *testing.T) {
	cases := []struct {
		entityType string
		want       string
	}{
		{"PurchaseOrder", "entity/PurchaseOrder"},
		{"Item", "entity/Item"},
		{"POLine", "entity/POLine"},
		{"", "entity/"},
	}
	for _, c := range cases {
		if got := TopicID(c.entityType); got != c.want {
			t.Errorf("TopicID(%q) = %q, want %q", c.entityType, got, c.want)
		}
	}
}

func TestRouteTopicID(t *testing.T) {
	// Mirrors slugifyTitle's edge cases in
	// internal/kernel/formrender/render.go (same collapse-to-underscore,
	// trim-leading/trailing rule) — this package can't import formrender
	// to share the function (the dependency runs the other way:
	// formrender calls into this package), so the rule is duplicated
	// deliberately, per ADR-0023, and this test is what keeps the two
	// implementations' behavior in sync if either one ever changes.
	cases := []struct {
		route string
		want  string
	}{
		{"dashboard", "route/dashboard"},
		{"Import Wizard", "route/import_wizard"},
		{"Lead-time stages", "route/lead_time_stages"},
		{"  Reports  ", "route/reports"},
		{"Admin & Setup", "route/admin_setup"},
		{"", "route/"},
	}
	for _, c := range cases {
		if got := RouteTopicID(c.route); got != c.want {
			t.Errorf("RouteTopicID(%q) = %q, want %q", c.route, got, c.want)
		}
	}
}

func TestHref(t *testing.T) {
	if got, want := Href("entity/Item"), "/help/entity/Item"; got != want {
		t.Errorf("Href(%q) = %q, want %q", "entity/Item", got, want)
	}
}

func TestHasContentIn(t *testing.T) {
	fsys := fstest.MapFS{
		"content/en/entity/Item.md":   {Data: []byte("# Item\n")},
		"content/tr/entity/Item.md":   {Data: []byte("# Ürün\n")},
		"content/en/route/reports.md": {Data: []byte("# Reports\n")},
	}

	cases := []struct {
		name    string
		locale  string
		topicID string
		want    bool
	}{
		{"exact locale match", "en", "entity/Item", true},
		{"non-English locale with its own file", "tr", "entity/Item", true},
		{"non-English locale falls back to en", "fa", "entity/Item", true},
		{"non-English locale falls back to en (ar)", "ar", "entity/Item", true},
		{"unknown topic, known locale", "en", "entity/DoesNotExist", false},
		{"unknown topic, unknown locale — no fallback either", "fa", "entity/DoesNotExist", false},
		{"route topic present", "en", "route/reports", true},
		{"unshipped locale still falls back to en", "fr", "route/reports", true}, // "fr" isn't one of this repo's four shipped locales, but the fallback to en still finds it
		{"empty topic id", "en", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasContentIn(fsys, c.locale, c.topicID); got != c.want {
				t.Errorf("hasContentIn(%s, %q) = %v, want %v", c.locale, c.topicID, got, c.want)
			}
		})
	}
}

func TestHasContentIn_EmptyFS(t *testing.T) {
	// The real embedded content/ directory ships with no per-locale
	// topics yet (ADR-0023: this slice establishes the convention, not
	// the content) — every topic must degrade to false against it, never
	// panic on a missing directory.
	fsys := fstest.MapFS{
		"content/README.md": {Data: []byte("# Help content\n")},
	}
	if hasContentIn(fsys, "en", "entity/PurchaseOrder") {
		t.Error("hasContentIn against a content root with no topics should be false")
	}
}

func TestHasContent_RealEmbeddedFS(t *testing.T) {
	// Confirms the actual shipped state as of ADR-0023 landing: no topic
	// has content yet, so HasContent must be false for everything,
	// including entity types that definitely exist in this kernel — this
	// is what makes formrender/listview's "?" render disabled everywhere
	// today, honestly, rather than by accident.
	if HasContent("en", TopicID("PurchaseOrder")) {
		t.Error("HasContent(\"en\", TopicID(\"PurchaseOrder\")) = true, want false — no help content ships in this slice")
	}
}
