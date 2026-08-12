package help

import (
	"strings"
	"testing"
	"testing/fstest"
)

func mdFile(data string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(data)}
}

func itemTopicEN() string {
	return "---\ntitle: Item\naudience: user\nmodule: inventory\norder: 10\n---\n# Item\n\nHow to manage items.\n"
}

func itemTopicTR() string {
	return "---\ntitle: Ürün\naudience: user\nmodule: inventory\norder: 10\n---\n# Ürün\n\nÜrünleri nasıl yönetilir.\n"
}

func adminReportTopicEN() string {
	return "---\ntitle: Admin Report\naudience: admin\nmodule: reporting\norder: 1\n---\nAdmin-only reporting content mentioning purchase orders.\n"
}

func TestBuildIndexFrom_MultiLocaleAndFallback(t *testing.T) {
	fsys := fstest.MapFS{
		"content/en/entity/Item.md": mdFile(itemTopicEN()),
		"content/tr/entity/Item.md": mdFile(itemTopicTR()),
		"content/README.md":         mdFile("# Help content\n"), // must not be treated as a topic
	}
	idx, err := BuildIndexFrom(fsys)
	if err != nil {
		t.Fatalf("BuildIndexFrom: %v", err)
	}

	enTopics := idx.Topics("en")
	if len(enTopics) != 1 {
		t.Fatalf("Topics(en) = %d topics, want 1", len(enTopics))
	}
	if enTopics[0].Title != "Item" {
		t.Errorf("Topics(en)[0].Title = %q, want %q", enTopics[0].Title, "Item")
	}

	trTopics := idx.Topics("tr")
	if len(trTopics) != 1 || trTopics[0].Title != "Ürün" {
		t.Fatalf("Topics(tr) = %+v, want tr's own translated title", trTopics)
	}

	// fa has no file for this topic — must fall back to en, same
	// direction help.HasContent already uses.
	faTopics := idx.Topics("fa")
	if len(faTopics) != 1 || faTopics[0].Title != "Item" {
		t.Fatalf("Topics(fa) = %+v, want fallback to the en title", faTopics)
	}
	if faTopics[0].Locale != "en" {
		t.Errorf("Topics(fa)[0].Locale = %q, want %q (the fallback source)", faTopics[0].Locale, "en")
	}
}

func TestBuildIndexFrom_ReadmeNotTreatedAsTopic(t *testing.T) {
	fsys := fstest.MapFS{
		"content/README.md": mdFile("# Help content\n"),
	}
	idx, err := BuildIndexFrom(fsys)
	if err != nil {
		t.Fatalf("BuildIndexFrom: %v", err)
	}
	if got := idx.Topics("en"); len(got) != 0 {
		t.Errorf("Topics(en) = %+v, want empty (README.md is not under any locale dir)", got)
	}
}

func TestBuildIndexFrom_EmptyFS(t *testing.T) {
	idx, err := BuildIndexFrom(fstest.MapFS{})
	if err != nil {
		t.Fatalf("BuildIndexFrom(empty): %v", err)
	}
	if got := idx.Topics("en"); len(got) != 0 {
		t.Errorf("Topics(en) on an empty index = %+v, want empty", got)
	}
}

func TestBuildIndexFrom_MalformedTopicFailsLoud(t *testing.T) {
	fsys := fstest.MapFS{
		"content/en/entity/Item.md": mdFile("no front matter here at all\n"),
	}
	_, err := BuildIndexFrom(fsys)
	if err == nil {
		t.Fatal("expected BuildIndexFrom to fail loud on a malformed topic file, got nil error")
	}
	if !strings.Contains(err.Error(), "content/en/entity/Item.md") {
		t.Errorf("error = %q, expected it to name the offending file", err)
	}
}

func TestBuildIndexFrom_OneMalformedTopicFailsTheWholeBuild(t *testing.T) {
	fsys := fstest.MapFS{
		"content/en/entity/Item.md":          mdFile(itemTopicEN()),
		"content/en/entity/PurchaseOrder.md": mdFile("---\ntitle: PO\naudience: user\nmodule: purchasing\norder: bad-order\n---\nbody\n"),
	}
	_, err := BuildIndexFrom(fsys)
	if err == nil {
		t.Fatal("expected one malformed topic to fail the entire index build, not be silently skipped")
	}
}

func TestIndex_Topics_SortedByModuleThenOrderThenTitle(t *testing.T) {
	fsys := fstest.MapFS{
		"content/en/entity/Zebra.md": mdFile("---\ntitle: Zebra\naudience: user\nmodule: a\norder: 1\n---\nx\n"),
		"content/en/entity/Apple.md": mdFile("---\ntitle: Apple\naudience: user\nmodule: a\norder: 1\n---\nx\n"),
		"content/en/entity/First.md": mdFile("---\ntitle: First\naudience: user\nmodule: a\norder: 0\n---\nx\n"),
		"content/en/entity/Other.md": mdFile("---\ntitle: Other\naudience: user\nmodule: b\norder: 0\n---\nx\n"),
	}
	idx, err := BuildIndexFrom(fsys)
	if err != nil {
		t.Fatalf("BuildIndexFrom: %v", err)
	}
	topics := idx.Topics("en")
	var order []string
	for _, tp := range topics {
		order = append(order, tp.Module+"/"+tp.Title)
	}
	want := []string{"a/First", "a/Apple", "a/Zebra", "b/Other"}
	if len(order) != len(want) {
		t.Fatalf("got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("Topics()[%d] = %q, want %q (full order: %v)", i, order[i], want[i], order)
		}
	}
}

func TestIndex_Get(t *testing.T) {
	fsys := fstest.MapFS{
		"content/en/entity/Item.md": mdFile(itemTopicEN()),
		"content/tr/entity/Item.md": mdFile(itemTopicTR()),
	}
	idx, err := BuildIndexFrom(fsys)
	if err != nil {
		t.Fatalf("BuildIndexFrom: %v", err)
	}

	r, ok := idx.Get("entity/Item", "tr")
	if !ok || r.Title != "Ürün" {
		t.Fatalf("Get(entity/Item, tr) = %+v, %v, want Ürün, true", r, ok)
	}

	r, ok = idx.Get("entity/Item", "fa")
	if !ok || r.Title != "Item" {
		t.Fatalf("Get(entity/Item, fa) = %+v, %v, want fallback to en Item, true", r, ok)
	}

	_, ok = idx.Get("entity/DoesNotExist", "en")
	if ok {
		t.Error("Get(entity/DoesNotExist, en) = true, want false")
	}
}

func TestIndex_OwnPlainText(t *testing.T) {
	fsys := fstest.MapFS{
		"content/en/entity/Item.md": mdFile(itemTopicEN()),
		"content/tr/entity/Item.md": mdFile(itemTopicTR()),
	}
	idx, err := BuildIndexFrom(fsys)
	if err != nil {
		t.Fatalf("BuildIndexFrom: %v", err)
	}

	if text, ok := idx.OwnPlainText("entity/Item", "en"); !ok || strings.TrimSpace(text) == "" {
		t.Fatalf("OwnPlainText(entity/Item, en) = %q, %v, want non-blank text, true", text, ok)
	}

	if text, ok := idx.OwnPlainText("entity/Item", "tr"); !ok || strings.TrimSpace(text) == "" {
		t.Fatalf("OwnPlainText(entity/Item, tr) = %q, %v, want non-blank text, true", text, ok)
	}

	// fa has no file of its own for this topic — unlike Get/resolve, this
	// must NOT fall back to en. This is the whole reason the method
	// exists: help-coverage-gate callers need to know fa itself has
	// nothing, not that something renders for a fa reader via fallback.
	if text, ok := idx.OwnPlainText("entity/Item", "fa"); ok {
		t.Errorf("OwnPlainText(entity/Item, fa) = %q, true, want ok=false — no fallback to en", text)
	}

	if _, ok := idx.OwnPlainText("entity/DoesNotExist", "en"); ok {
		t.Error("OwnPlainText(entity/DoesNotExist, en) = true, want false")
	}
}

func TestIndex_OwnPlainText_EmptyBodyStub(t *testing.T) {
	// A topic file with front matter but no real body must not read as
	// "documented" to a caller that (correctly) checks the returned text
	// for non-blank content — OwnPlainText itself just reports what the
	// file parsed to; a front-matter-only stub parses fine and legitimately
	// returns ok=true with empty PlainText, and it's the caller's job (the
	// help-coverage gate) to treat empty text as undocumented, same as
	// this repo's i18n coverage gate treats a blank catalog value as
	// untranslated rather than present.
	fsys := fstest.MapFS{
		"content/en/entity/Item.md": mdFile("---\ntitle: Item\naudience: user\nmodule: inventory\norder: 1\n---\n"),
	}
	idx, err := BuildIndexFrom(fsys)
	if err != nil {
		t.Fatalf("BuildIndexFrom: %v", err)
	}
	text, ok := idx.OwnPlainText("entity/Item", "en")
	if !ok {
		t.Fatal("OwnPlainText(entity/Item, en) ok = false, want true (file exists and parses)")
	}
	if strings.TrimSpace(text) != "" {
		t.Fatalf("OwnPlainText(entity/Item, en) = %q, want blank (stub has no body)", text)
	}
}

func TestContentLocales(t *testing.T) {
	got := ContentLocales()
	want := []string{"en", "ar", "fa", "tr"}
	if len(got) != len(want) {
		t.Fatalf("ContentLocales() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ContentLocales() = %v, want %v", got, want)
		}
	}
	// Mutating the returned slice must not affect the package's own
	// source of truth — ContentLocales returns a copy, not the live
	// contentLocales slice, so a later call must still see the real set.
	got[0] = "xx"
	if got2 := ContentLocales(); got2[0] != "en" {
		t.Fatalf("ContentLocales() after mutating a previous result = %v, want unaffected (%q at index 0)", got2, "en")
	}
}

func TestIndex_Search_EmptyQueryReturnsNilNotError(t *testing.T) {
	idx, err := BuildIndexFrom(fstest.MapFS{"content/en/entity/Item.md": mdFile(itemTopicEN())})
	if err != nil {
		t.Fatalf("BuildIndexFrom: %v", err)
	}
	if got := idx.Search("en", ""); got != nil {
		t.Errorf("Search(en, \"\") = %+v, want nil", got)
	}
	if got := idx.Search("en", "   "); got != nil {
		t.Errorf("Search(en, whitespace) = %+v, want nil", got)
	}
}

func TestIndex_Search_EmptyIndexReturnsNilNotPanic(t *testing.T) {
	idx, err := BuildIndexFrom(fstest.MapFS{})
	if err != nil {
		t.Fatalf("BuildIndexFrom: %v", err)
	}
	if got := idx.Search("en", "anything"); got != nil {
		t.Errorf("Search on empty index = %+v, want nil", got)
	}
}

func TestIndex_Search_TitleMatchesRankBeforeBodyMatches(t *testing.T) {
	fsys := fstest.MapFS{
		"content/en/entity/Item.md":          mdFile(itemTopicEN()), // title "Item", body mentions "items"
		"content/en/entity/PurchaseOrder.md": mdFile("---\ntitle: Purchase Order\naudience: user\nmodule: purchasing\norder: 1\n---\nHow to order an item for the warehouse.\n"),
	}
	idx, err := BuildIndexFrom(fsys)
	if err != nil {
		t.Fatalf("BuildIndexFrom: %v", err)
	}
	results := idx.Search("en", "item")
	if len(results) != 2 {
		t.Fatalf("Search(item) = %+v, want 2 matches", results)
	}
	if results[0].ID != "entity/Item" {
		t.Errorf("Search(item)[0] = %q, want the title match (entity/Item) ranked first", results[0].ID)
	}
	if results[1].ID != "entity/PurchaseOrder" {
		t.Errorf("Search(item)[1] = %q, want the body-only match ranked second", results[1].ID)
	}
}

func TestIndex_Search_NoMatches(t *testing.T) {
	idx, err := BuildIndexFrom(fstest.MapFS{"content/en/entity/Item.md": mdFile(itemTopicEN())})
	if err != nil {
		t.Fatalf("BuildIndexFrom: %v", err)
	}
	if got := idx.Search("en", "zzz-does-not-exist"); len(got) != 0 {
		t.Errorf("Search(no match) = %+v, want empty", got)
	}
}

func TestIndex_Search_CaseInsensitive(t *testing.T) {
	idx, err := BuildIndexFrom(fstest.MapFS{"content/en/entity/Item.md": mdFile(itemTopicEN())})
	if err != nil {
		t.Fatalf("BuildIndexFrom: %v", err)
	}
	for _, q := range []string{"item", "ITEM", "ItEm"} {
		if got := idx.Search("en", q); len(got) != 1 {
			t.Errorf("Search(%q) = %+v, want 1 case-insensitive match", q, got)
		}
	}
}

func TestFoldForSearch_DiacriticInsensitive_Arabic(t *testing.T) {
	// Arabic tashkil (diacritics) are combining marks (Unicode Mn) added
	// on top of base letters — a search for the undiacritized form
	// should still match diacritized stored text.
	diacritized := "مَرْحَبًا"
	plain := "مرحبا"
	if fold := foldForSearch(diacritized); fold != foldForSearch(plain) {
		t.Errorf("foldForSearch(%q) = %q, foldForSearch(%q) = %q, want equal after diacritic stripping", diacritized, fold, plain, foldForSearch(plain))
	}
}

func TestFoldForSearch_DiacriticInsensitive_Farsi(t *testing.T) {
	diacritized := "سَلام" // salām with a fatha diacritic
	plain := "سلام"
	if fold := foldForSearch(diacritized); fold != foldForSearch(plain) {
		t.Errorf("foldForSearch(%q) = %q, foldForSearch(%q) = %q, want equal after diacritic stripping", diacritized, fold, plain, foldForSearch(plain))
	}
}

func TestFoldForSearch_Turkish_DottedCapitalIFoldsLikeLowercaseI(t *testing.T) {
	// "İ" (U+0130, dotted capital I) NFKD-decomposes to "I" + a
	// combining dot above (U+0307); after stripping combining marks and
	// lowercasing, it must fold the same way "i" already does, so a
	// query typed as "izmir" still matches a title stored as "İzmir".
	if fold := foldForSearch("İzmir"); fold != foldForSearch("izmir") {
		t.Errorf("foldForSearch(%q) = %q, want equal to foldForSearch(%q) = %q", "İzmir", fold, "izmir", foldForSearch("izmir"))
	}
}

func TestIndex_Search_DiacriticFoldedMatch(t *testing.T) {
	fsys := fstest.MapFS{
		"content/fa/entity/Item.md": mdFile("---\ntitle: سَلام\naudience: user\nmodule: inventory\norder: 1\n---\nx\n"),
		"content/en/entity/Item.md": mdFile(itemTopicEN()),
	}
	idx, err := BuildIndexFrom(fsys)
	if err != nil {
		t.Fatalf("BuildIndexFrom: %v", err)
	}
	got := idx.Search("fa", "سلام")
	if len(got) != 1 {
		t.Fatalf("Search(fa, undiacritized query) = %+v, want 1 diacritic-folded match", got)
	}
}

func TestGetIndex_RealEmbeddedContent(t *testing.T) {
	idx, err := GetIndex()
	if err != nil {
		t.Fatalf("GetIndex: %v", err)
	}
	// uc-infra#147 shipped the first real topic content (the foundation
	// module, in all four locales) — the production index must build
	// successfully and actually contain it. Checked via one known-stable
	// topic (entity/Party, foundation's own canonical example) rather than
	// an exact count, since later content slices (uc-infra#148-152) will
	// keep adding topics without this test needing to track the total.
	rendered, ok := idx.Get("entity/Party", "en")
	if !ok || strings.TrimSpace(rendered.PlainText) == "" {
		t.Fatalf("GetIndex().Get(entity/Party, en) = (%+v, %v), want real, non-blank content now that uc-infra#147 has shipped it", rendered, ok)
	}
}

func TestSetIndexForTesting_OverridesAndRestores(t *testing.T) {
	fixture, err := BuildIndexFrom(fstest.MapFS{"content/en/entity/Item.md": mdFile(itemTopicEN())})
	if err != nil {
		t.Fatalf("BuildIndexFrom: %v", err)
	}
	restore := SetIndexForTesting(fixture)
	got, err := GetIndex()
	if err != nil {
		t.Fatalf("GetIndex: %v", err)
	}
	if got != fixture {
		t.Fatalf("GetIndex() during override = %p, want the fixture index %p", got, fixture)
	}
	if len(got.Topics("en")) != 1 {
		t.Fatalf("GetIndex().Topics(en) during override = %+v, want the fixture's 1 topic", got.Topics("en"))
	}
	restore()

	after, err := GetIndex()
	if err != nil {
		t.Fatalf("GetIndex after restore: %v", err)
	}
	// The restored index must be the real production index again, not the
	// fixture — checked by identity (not by an empty topic count: since
	// uc-infra#147 the real index actually ships real content, so "empty"
	// is no longer what "restored" means) and by containing the real
	// entity/Party topic the fixture above never defined.
	if after == fixture {
		t.Fatalf("GetIndex() after restore = the fixture index %p, want the real production index back", fixture)
	}
	if _, ok := after.Get("entity/Party", "en"); !ok {
		t.Errorf("GetIndex().Get(entity/Party, en) after restore: not found, want the real production index's own content back")
	}
}

func TestSortTopics_StableOnEqualKeys(t *testing.T) {
	// Two topics with the identical (Module, Order, Title) sort key —
	// stability means their relative input order survives.
	a := Rendered{Topic: Topic{ID: "a", Module: "m", Order: 1, Title: "Same"}}
	b := Rendered{Topic: Topic{ID: "b", Module: "m", Order: 1, Title: "Same"}}
	rs := []Rendered{a, b}
	sortTopics(rs)
	if rs[0].ID != "a" || rs[1].ID != "b" {
		t.Errorf("sortTopics on equal keys = %v, want stable order [a b]", []string{rs[0].ID, rs[1].ID})
	}
}
