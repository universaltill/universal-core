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

func TestAssetIn(t *testing.T) {
	fsys := fstest.MapFS{
		"content/assets/list/en.jpg": {Data: []byte("fake-jpeg-bytes")},
	}

	data, ok := assetIn(fsys, "list/en.jpg")
	if !ok {
		t.Fatal("expected list/en.jpg to be found")
	}
	if string(data) != "fake-jpeg-bytes" {
		t.Errorf("data = %q, want %q", data, "fake-jpeg-bytes")
	}

	if _, ok := assetIn(fsys, "list/missing.jpg"); ok {
		t.Error("expected a missing asset to report ok=false")
	}
	if _, ok := assetIn(fsys, ""); ok {
		t.Error("expected an empty path to report ok=false")
	}
}

// TestAssetIn_PathTraversalRejected is the regression proof for Asset's
// own doc comment: a "../" path element must never escape content/assets
// — fs.ReadFile against an fs.FS validates the joined name via
// fs.ValidPath and refuses it outright, so this never reaches a real
// filesystem read outside the embedded tree.
func TestAssetIn_PathTraversalRejected(t *testing.T) {
	fsys := fstest.MapFS{
		"content/assets/list/en.jpg": {Data: []byte("fake-jpeg-bytes")},
		"content/README.md":          {Data: []byte("# Help content\n")},
	}
	if _, ok := assetIn(fsys, "../README.md"); ok {
		t.Error("expected a \"../\" path element to be rejected, not escape content/assets")
	}
	if _, ok := assetIn(fsys, "../../help.go"); ok {
		t.Error("expected a multi-level \"../\" path to be rejected")
	}
}

func TestAsset_RealEmbeddedFS(t *testing.T) {
	// Confirms the real captured screenshots (uc-infra#145,
	// internal/e2e's TestCaptureHelpScreenshots) actually shipped —
	// 4 layout families x 2 locales, en always present per family.
	for _, family := range []string{"list", "form", "help", "wizard"} {
		data, ok := Asset(family + "/en.jpg")
		if !ok {
			t.Errorf("Asset(%q) not found — run CAPTURE_HELP_SCREENSHOTS=1 go test ./internal/e2e -run TestCaptureHelpScreenshots -v", family+"/en.jpg")
			continue
		}
		if len(data) == 0 {
			t.Errorf("Asset(%q) returned 0 bytes", family+"/en.jpg")
		}
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
