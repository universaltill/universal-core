package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
)

// unitEntityDef is a controlled-vocabulary target whose label field `name`
// is multilingual (i18n_text, ADR-0009) — the shape #23 exists to serve.
func unitI18nEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "MultiUnit",
		Version:    1,
		Fields:     []entity.Field{{Name: "name", Type: entity.FieldI18nText, Required: true}},
	}
}

func unitI18nFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "MultiUnit",
		Version:    1,
		Sections: []form.Section{{
			Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name", Label: "Name"}},
		}},
	}
}

// widgetRefEntityDef references MultiUnit, so its picker/list must resolve
// the multilingual label.
func widgetRefEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Widget2",
		Version:    1,
		Fields: []entity.Field{
			{Name: "sku", Type: entity.FieldString, Required: true},
			{Name: "unit_id", Type: entity.FieldReference, Target: "MultiUnit"},
		},
	}
}

func widgetRefFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "Widget2",
		Version:    1,
		Sections: []form.Section{{
			Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "sku", Label: "SKU"}, {Name: "unit_id", Label: "Unit"}},
		}},
	}
}

func postForm(t *testing.T, mux *http.ServeMux, target, tenantID, actorID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest("POST", target, tenantID, actorID, []byte(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestAPI_I18nText_FormAssemblesObjectAndRoundTrips proves the HTTP form
// decoder reassembles the per-locale inputs (name.en, name.tr) into the
// stored locale->string object, and that it round-trips back out.
func TestAPI_I18nText_FormAssemblesObjectAndRoundTrips(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, unitI18nEntityDef(), unitI18nFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rec := postForm(t, mux, "/api/records/MultiUnit", tenantID, "farshid", "name.en=Each&name.tr=Adet")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create MultiUnit via form: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			ID   string         `json:"id"`
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	nameObj, ok := created.Data.Data["name"].(map[string]any)
	if !ok {
		t.Fatalf("expected name to be stored as an object, got %T: %#v", created.Data.Data["name"], created.Data.Data)
	}
	if nameObj["en"] != "Each" || nameObj["tr"] != "Adet" {
		t.Fatalf("expected {en:Each, tr:Adet}, got %#v", nameObj)
	}

	// The edit form pre-fills each locale input from the stored object.
	form := getAs(t, mux, "/forms/MultiUnit/"+created.Data.ID, tenantID, "farshid")
	if !strings.Contains(form.Body.String(), `name="name.en" value="Each"`) ||
		!strings.Contains(form.Body.String(), `name="name.tr" value="Adet"`) {
		t.Fatalf("edit form did not pre-fill per-locale inputs:\n%s", form.Body.String())
	}
}

// TestAPI_I18nText_RequiredFieldRejectsBlankOverJSONAPI is the JSON-API
// half of uc-infra#104 (split from #86, whose title "Required does not
// reject an empty string over the JSON API" reads as fully closed for the
// kernel but wasn't for this one field type): a required i18n_text field
// submitted as {} or with every locale blank must 400, the same as an
// absent field already does — not silently create a record with no
// translation in any language.
func TestAPI_I18nText_RequiredFieldRejectsBlankOverJSONAPI(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, unitI18nEntityDef(), unitI18nFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty object", `{"name":{}}`},
		{"single blank locale", `{"name":{"en":""}}`},
		{"every locale blank", `{"name":{"en":"","tr":""}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := newRequest("POST", "/api/records/MultiUnit", tenantID, "farshid", []byte(tc.body))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for a required i18n_text field with no content in any locale, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}

	// A non-blank locale still creates fine — this isn't rejecting the
	// field outright, only "no content anywhere".
	rec := httptest.NewRecorder()
	req := newRequest("POST", "/api/records/MultiUnit", tenantID, "farshid", []byte(`{"name":{"en":"","tr":"Adet"}}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 when at least one locale has content, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_I18nText_ReferenceLabelResolvesViewerLocale is the end-to-end
// point of #23: a reference picker, its search endpoint, and the list cell
// all show the target's label in the VIEWER's locale.
func TestAPI_I18nText_ReferenceLabelResolvesViewerLocale(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, unitI18nEntityDef(), unitI18nFormDef())
	publishEntityAndForm(t, db, widgetRefEntityDef(), widgetRefFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Create the multilingual unit.
	unitRec := postForm(t, mux, "/api/records/MultiUnit", tenantID, "farshid", "name.en=Each&name.tr=Adet")
	if unitRec.Code != http.StatusCreated {
		t.Fatalf("create unit: %d: %s", unitRec.Code, unitRec.Body.String())
	}
	var unit struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(unitRec.Body.Bytes(), &unit)

	// The search endpoint labels by the viewer's locale.
	for _, tc := range []struct{ lang, want string }{{"tr", "Adet"}, {"en", "Each"}} {
		rec := getAs(t, mux, "/api/references/MultiUnit?lang="+tc.lang, tenantID, "farshid")
		if rec.Code != http.StatusOK {
			t.Fatalf("reference search lang=%s: %d: %s", tc.lang, rec.Code, rec.Body.String())
		}
		opts := decodeRefOptions(t, rec.Body.Bytes())
		if len(opts) != 1 || opts[0].ID != unit.Data.ID || opts[0].Label != tc.want {
			t.Fatalf("reference search lang=%s: want label %q, got %+v", tc.lang, tc.want, opts)
		}
	}

	// A Widget2 referencing the unit: its list cell shows the localized
	// label, not a raw id or the JSON object.
	wRec := postForm(t, mux, "/api/records/Widget2", tenantID, "farshid", "sku=W-1&unit_id="+unit.Data.ID)
	if wRec.Code != http.StatusCreated {
		t.Fatalf("create widget: %d: %s", wRec.Code, wRec.Body.String())
	}
	list := getAs(t, mux, "/records/Widget2?lang=tr", tenantID, "farshid")
	if list.Code != http.StatusOK {
		t.Fatalf("list lang=tr: %d: %s", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), "Adet") {
		t.Fatalf("expected the tr unit label 'Adet' in the list cell, got:\n%s", list.Body.String())
	}
	if strings.Contains(list.Body.String(), unit.Data.ID) {
		t.Fatalf("list cell leaked the raw unit id instead of a label:\n%s", list.Body.String())
	}
}

// localizedThingEntityDef mixes a plain, sortable `code` with a
// multilingual `name` (i18n_text) so its own list page exercises both the
// localized-cell rendering and the sort/filter degradation.
func localizedThingEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "LocalizedThing",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldI18nText, Required: true},
			{Name: "code", Type: entity.FieldString},
		},
	}
}

func localizedThingFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "LocalizedThing",
		Version:    1,
		Sections: []form.Section{{Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name", Label: "Name"}, {Name: "code", Label: "Code"}}}},
	}
}

// TestAPI_I18nText_OwnListPageLocalizesAndDegrades is the regression test
// for the review finding that an entity's OWN list column of type i18n_text
// rendered as a raw `map[en:Each tr:Adet]` (not localized), and that the
// list page's sort/filter didn't degrade for such a column (unlike the
// reference-search endpoint). Covers the surface the reference-only tests
// missed.
func TestAPI_I18nText_OwnListPageLocalizesAndDegrades(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, localizedThingEntityDef(), localizedThingFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	postForm(t, mux, "/api/records/LocalizedThing", tenantID, "farshid", "name.en=Each&name.tr=Adet&code=EA")
	postForm(t, mux, "/api/records/LocalizedThing", tenantID, "farshid", "name.en=Box&name.tr=Kutu&code=BX")

	// The entity's own list page, viewed in Turkish: the name column shows
	// the localized value, never a raw map dump or the English value.
	list := getAs(t, mux, "/records/LocalizedThing?lang=tr", tenantID, "farshid")
	if list.Code != http.StatusOK {
		t.Fatalf("list: %d: %s", list.Code, list.Body.String())
	}
	body := list.Body.String()
	if strings.Contains(body, "map[") {
		t.Fatalf("i18n_text column rendered as a raw Go map, not localized:\n%s", body)
	}
	if !strings.Contains(body, "Adet") || !strings.Contains(body, "Kutu") {
		t.Fatalf("expected the Turkish labels in the name column, got:\n%s", body)
	}
	// Check the English values in cell context (">Each<") — a bare
	// "Box"/"Each" substring also matches the shell's "combobox" JS.
	if strings.Contains(body, ">Each<") || strings.Contains(body, ">Box<") {
		t.Fatalf("Turkish list leaked the English i18n values, got:\n%s", body)
	}

	// The i18n_text column header must NOT be a sort link (can't order a
	// JSON object); the plain `code` column still is. Matched as a
	// standalone "sort=name" query param (bounded by "?"/"&" and "&"/
	// the closing quote), not a raw prefix substring: uc-infra#164 added
	// a "qregion" param that url.Values.Encode() sorts ahead of "sort"
	// alphabetically, so a plain `?sort=name` prefix check would never
	// match any real generated href again, silently defanging this
	// regression guard (independent review).
	sortNameParam := regexp.MustCompile(`[?&]sort=name(?:&|")`)
	if sortNameParam.MatchString(body) {
		t.Fatalf("i18n_text column must not offer a sort link, got:\n%s", body)
	}
	if !strings.Contains(body, "sort=code") {
		t.Fatalf("the plain code column should still be sortable, got:\n%s", body)
	}

	// Sorting by the i18n_text column is ignored (degraded), not a 500.
	if s := getAs(t, mux, "/records/LocalizedThing?sort=name&lang=tr", tenantID, "farshid"); s.Code != http.StatusOK {
		t.Fatalf("sort by i18n_text column should degrade to 200, got %d: %s", s.Code, s.Body.String())
	}
	// Filtering explicitly by the i18n_text column is ignored (both rows
	// remain), not a wrong substring match on the JSON text.
	f := getAs(t, mux, "/records/LocalizedThing?filter=name&q=Each&lang=en", tenantID, "farshid")
	if f.Code != http.StatusOK {
		t.Fatalf("filter by i18n_text column should degrade to 200, got %d", f.Code)
	}
	if !strings.Contains(f.Body.String(), "EA") || !strings.Contains(f.Body.String(), "BX") {
		t.Fatalf("filtering by an i18n_text column must not narrow (degraded), expected both rows, got:\n%s", f.Body.String())
	}
	// The plain code column still filters for real.
	fc := getAs(t, mux, "/records/LocalizedThing?filter=code&q=EA&lang=en", tenantID, "farshid")
	if !strings.Contains(fc.Body.String(), "EA") || strings.Contains(fc.Body.String(), "BX") {
		t.Fatalf("filtering by the plain code column should narrow to EA only, got:\n%s", fc.Body.String())
	}
}

// TestAPI_I18nText_SearchFiltersAndSortsByViewerLocale is uc-infra#249
// item 1: the reference-picker search endpoint used to hard-degrade to an
// unsorted, unfiltered capped listing whenever the target's label field
// was i18n_text (ADR-0009) — this is the regression test proving that gap
// is closed. It both filters (a query matching only a non-default-locale
// translation returns that record, and nothing else) and sorts (by the
// viewer's own resolved label, not creation order) using
// ListPageOptions.FilterI18nText/SortI18nLocales.
func TestAPI_I18nText_SearchFiltersAndSortsByViewerLocale(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, unitI18nEntityDef(), unitI18nFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Created out of alphabetical order, so a passing sort assertion can't
	// be explained away by creation order happening to already match.
	postForm(t, mux, "/api/records/MultiUnit", tenantID, "farshid", "name.en=Zulu&name.tr=Onaylandı")
	postForm(t, mux, "/api/records/MultiUnit", tenantID, "farshid", "name.en=Alpha&name.tr=Beta")

	// Filtering: "onay" is a substring only of the Turkish translation
	// "Onaylandı" (the record whose English label is "Zulu") — a query
	// matching a non-default-locale translation must return that record,
	// and only that record.
	rec := getAs(t, mux, "/api/references/MultiUnit?q=onay&lang=en", tenantID, "farshid")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].Label != "Zulu" {
		t.Fatalf("expected exactly the Zulu/Onaylandı unit (matched via its Turkish translation), got %+v", opts)
	}

	// Sorting: under lang=tr, the two units' Turkish labels are "Beta" and
	// "Onaylandı" — alphabetically Beta first — which is NOT creation
	// order (Onaylandı/Zulu was created first above).
	sortRec := getAs(t, mux, "/api/references/MultiUnit?lang=tr", tenantID, "farshid")
	if sortRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", sortRec.Code, sortRec.Body.String())
	}
	sorted := decodeRefOptions(t, sortRec.Body.Bytes())
	if len(sorted) != 2 || sorted[0].Label != "Beta" || sorted[1].Label != "Onaylandı" {
		t.Fatalf("expected [Beta, Onaylandı] sorted by the tr label (not creation order), got %+v", sorted)
	}
}

// widgetRefTargetFirstEntityDef/FormDef is widgetRefEntityDef's target
// field reordered to be FIRST (matching the real production shape:
// Task.project_id, TimeEntry.task_id, ProjectBudgetLine.project_id and
// DepreciationSchedule.fixed_asset_id are all their own entity's first
// field, all targeting an i18n_text-labelled entity) — needed to exercise
// renderRecordList's IMPLICIT default-filter-column path (no explicit
// ?filter=), not just the explicit one widgetRefEntityDef's "sku first"
// shape reaches.
func widgetRefTargetFirstEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Widget3",
		Version:    1,
		Fields: []entity.Field{
			{Name: "unit_id", Type: entity.FieldReference, Target: "MultiUnit"},
			{Name: "sku", Type: entity.FieldString, Required: true},
		},
	}
}

func widgetRefTargetFirstFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "Widget3",
		Version:    1,
		Sections: []form.Section{{
			Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "unit_id", Label: "Unit"}, {Name: "sku", Label: "SKU"}},
		}},
	}
}

// TestListPage_ReferenceFilter_I18nTextTargetLabelMatchesRealPerLocaleLabel
// is the regression test for uc-infra#245's correctness bug: unlike
// reference_search.go's own canSortFilter guard (exercised above by
// TestAPI_I18nText_SearchDegradesSortFilter), listview.go's
// referenceIDsMatching had NO equivalent guard — filtering a list page by
// a FieldReference column whose TARGET's label field is i18n_text (ADR-
// 0009) built `data->>labelField ILIKE '%text%'` straight against the
// target's raw JSON blob, not the viewer's actual displayed label.
//
// The sharpest proof of the OLD bug: every i18n_text-labelled record's raw
// JSON contains the literal locale key "en" — so filtering by q=en used to
// match EVERY row regardless of what its real (English-locale) label
// actually says.
//
// The fix does NOT simply disable this filter (an earlier draft did,
// independent review caught that this silently regresses board #49's
// shipped label-filter feature on every entity whose first/only reachable
// filter column is a reference to an i18n_text-labelled target — a real,
// common shape, not a hypothetical one: see widgetRefTargetFirstEntityDef's
// own doc comment for the production examples). Instead it resolves the
// match application-side, against each candidate's REAL per-viewer-locale
// label — the same resolution the list cell itself renders, so what
// matches here is exactly what's on screen.
func TestListPage_ReferenceFilter_I18nTextTargetLabelMatchesRealPerLocaleLabel(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, unitI18nEntityDef(), unitI18nFormDef())
	publishEntityAndForm(t, db, widgetRefEntityDef(), widgetRefFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	eachUnit := postForm(t, mux, "/api/records/MultiUnit", tenantID, "farshid", "name.en=Each&name.tr=Adet")
	boxUnit := postForm(t, mux, "/api/records/MultiUnit", tenantID, "farshid", "name.en=Box&name.tr=Kutu")
	var each, box struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(eachUnit.Body.Bytes(), &each)
	json.Unmarshal(boxUnit.Body.Bytes(), &box)

	w1 := postForm(t, mux, "/api/records/Widget2", tenantID, "farshid", "sku=W-1&unit_id="+each.Data.ID)
	w2 := postForm(t, mux, "/api/records/Widget2", tenantID, "farshid", "sku=W-2&unit_id="+box.Data.ID)
	if w1.Code != http.StatusCreated || w2.Code != http.StatusCreated {
		t.Fatalf("seed widgets: W-1 %d, W-2 %d", w1.Code, w2.Code)
	}

	// The literal-locale-key false positive: q=en is nobody's real label
	// ("Each"/"Box" contain no "en" substring), but every MultiUnit's raw
	// JSON contains the key "en" — before the fix this matched BOTH
	// widgets. Also rules out a regressed nil-vs-empty FilterIn (a nil
	// return here would degrade to an ILIKE against the raw stored
	// unit_id UUID, which "en" could in principle match by chance — the
	// positive per-locale assertions below are what actually pin that
	// distinction deterministically, since a nil-degrade could never
	// match "Each"/"Box"/"Kutu" against a hex UUID).
	byKey := getAs(t, mux, "/records/Widget2?filter=unit_id&q=en", tenantID, "farshid")
	if byKey.Code != http.StatusOK {
		t.Fatalf("filter by i18n_text target label should return 200, got %d: %s", byKey.Code, byKey.Body.String())
	}
	if strings.Contains(byKey.Body.String(), "W-1") || strings.Contains(byKey.Body.String(), "W-2") {
		t.Fatalf("q=en (a locale key, not anyone's real label) must match no widget:\n%s", byKey.Body.String())
	}

	// A real label value must find exactly the ONE widget whose target
	// actually carries it — not both (proving this isn't a raw-JSON
	// substring match, which would also incidentally work here) and not
	// neither (proving the filter isn't just disabled). Only a genuine
	// per-record label comparison produces this exact result: a nil/
	// degraded FilterIn could never match "Each" against a hex UUID
	// either, so this also fails loudly if that regresses.
	byRealLabel := getAs(t, mux, "/records/Widget2?filter=unit_id&q=Each", tenantID, "farshid")
	if byRealLabel.Code != http.StatusOK {
		t.Fatalf("filter by i18n_text target label should return 200, got %d: %s", byRealLabel.Code, byRealLabel.Body.String())
	}
	if !strings.Contains(byRealLabel.Body.String(), "W-1") {
		t.Fatalf("q=Each should find W-1 (its unit's real English label):\n%s", byRealLabel.Body.String())
	}
	if strings.Contains(byRealLabel.Body.String(), "W-2") {
		t.Fatalf("q=Each should NOT find W-2 (Box/Kutu, unrelated label):\n%s", byRealLabel.Body.String())
	}

	// The match is against the VIEWER'S locale, not always English: under
	// lang=tr, Box's displayed label is "Kutu" — q=Box (the English value)
	// must NOT match under tr, and q=Kutu (the Turkish value) must.
	byEnUnderTr := getAs(t, mux, "/records/Widget2?filter=unit_id&q=Box&lang=tr", tenantID, "farshid")
	if strings.Contains(byEnUnderTr.Body.String(), "W-2") {
		t.Fatalf("q=Box under lang=tr should not match — the tr viewer sees \"Kutu\", not \"Box\":\n%s", byEnUnderTr.Body.String())
	}
	byTrLabel := getAs(t, mux, "/records/Widget2?filter=unit_id&q=Kutu&lang=tr", tenantID, "farshid")
	if !strings.Contains(byTrLabel.Body.String(), "W-2") {
		t.Fatalf("q=Kutu under lang=tr should match W-2 (Box unit's real Turkish label):\n%s", byTrLabel.Body.String())
	}
	if strings.Contains(byTrLabel.Body.String(), "W-1") {
		t.Fatalf("q=Kutu under lang=tr should not also match W-1 (Each/Adet, unrelated label):\n%s", byTrLabel.Body.String())
	}

	// Sanity control: the SAME filter mechanism still narrows correctly
	// for a plain-string (non-i18n) reference target, proving the above
	// isn't just a broken query — see
	// TestListPage_DefaultFilterOnReferenceColumn_ResolvesByLabel for the
	// full version of this control.
	sku := getAs(t, mux, "/records/Widget2?filter=sku&q=W-1", tenantID, "farshid")
	if !strings.Contains(sku.Body.String(), "W-1") || strings.Contains(sku.Body.String(), "W-2") {
		t.Fatalf("plain-string filter (sku) should still narrow correctly:\n%s", sku.Body.String())
	}
}

// TestListPage_DefaultFilterOnReferenceColumn_I18nTextTargetLabel is the
// companion regression test for the IMPLICIT default-filter-column path
// (no explicit ?filter= in the URL): widgetRefEntityDef puts the plain-
// string "sku" first, so the test above never actually exercises the
// production shape where the reference-to-i18n_text-target column is
// itself the default (Task/TimeEntry/ProjectBudgetLine/
// DepreciationSchedule all have this shape for real — independent review).
// widgetRefTargetFirstEntityDef puts "unit_id" first specifically to close
// that gap.
func TestListPage_DefaultFilterOnReferenceColumn_I18nTextTargetLabel(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, unitI18nEntityDef(), unitI18nFormDef())
	publishEntityAndForm(t, db, widgetRefTargetFirstEntityDef(), widgetRefTargetFirstFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	eachUnit := postForm(t, mux, "/api/records/MultiUnit", tenantID, "farshid", "name.en=Each&name.tr=Adet")
	boxUnit := postForm(t, mux, "/api/records/MultiUnit", tenantID, "farshid", "name.en=Box&name.tr=Kutu")
	var each, box struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(eachUnit.Body.Bytes(), &each)
	json.Unmarshal(boxUnit.Body.Bytes(), &box)

	w1 := postForm(t, mux, "/api/records/Widget3", tenantID, "farshid", "sku=W-1&unit_id="+each.Data.ID)
	w2 := postForm(t, mux, "/api/records/Widget3", tenantID, "farshid", "sku=W-2&unit_id="+box.Data.ID)
	if w1.Code != http.StatusCreated || w2.Code != http.StatusCreated {
		t.Fatalf("seed widgets: W-1 %d, W-2 %d", w1.Code, w2.Code)
	}

	// No ?filter= at all: the box defaults to "unit_id" (the first
	// column), a Reference whose target's label is i18n_text.
	body := getAs(t, mux, "/records/Widget3?q=Each", tenantID, "farshid").Body.String()
	if !strings.Contains(body, `name="filter" value="unit_id"`) {
		t.Fatalf("expected the default filter field to resolve to the reference column \"unit_id\":\n%s", body)
	}
	if !strings.Contains(body, "W-1") {
		t.Fatalf("default filter q=Each on an i18n_text-labelled reference target should find W-1:\n%s", body)
	}
	if strings.Contains(body, "W-2") {
		t.Fatalf("default filter q=Each should not also match W-2 (Box/Kutu, unrelated label):\n%s", body)
	}

	// Same discriminating check as the explicit-?filter= test: q=en (a
	// locale key baked into every i18n_text record's raw JSON, not
	// anyone's real label) must match NEITHER widget. Under the old
	// unguarded raw-JSON ILIKE this matched everything; it's included here
	// too because the positive q=Each check above (unlike this one) would
	// have passed by ACCIDENT even against the old, unfixed code — Each's
	// own raw JSON blob also happens to literally contain the substring
	// "Each" — so that assertion alone doesn't actually prove this test
	// exercises the fix rather than the pre-existing bug.
	byKey := getAs(t, mux, "/records/Widget3?q=en", tenantID, "farshid").Body.String()
	if strings.Contains(byKey, "W-1") || strings.Contains(byKey, "W-2") {
		t.Fatalf("default filter q=en (a locale key) must match no widget:\n%s", byKey)
	}
}

// TestAPI_RecordLabel_I18nTextLegacyPlainStringFallsThroughNotRawID is the
// regression test for the second, narrower finding in uc-infra#245:
// Handler.recordLabel returned the raw record UUID whenever an i18n_text
// field's ResolveLocalized failed to find a usable translation — including
// when the stored value isn't a locale->string object at all (a legacy row
// from before this field's own string->i18n_text migration, still holding
// a plain string). The fix falls through to the plain-string branch
// instead of returning the id outright, so a legacy value still renders as
// itself.
func TestAPI_RecordLabel_I18nTextLegacyPlainStringFallsThroughNotRawID(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, unitI18nEntityDef(), unitI18nFormDef())
	publishEntityAndForm(t, db, widgetRefEntityDef(), widgetRefFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	unitRec := postForm(t, mux, "/api/records/MultiUnit", tenantID, "farshid", "name.en=Each&name.tr=Adet")
	if unitRec.Code != http.StatusCreated {
		t.Fatalf("create unit: %d: %s", unitRec.Code, unitRec.Body.String())
	}
	var unit struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(unitRec.Body.Bytes(), &unit)

	// Simulate a legacy row that predates this field's string->i18n_text
	// migration: overwrite the stored value with a bare JSON string
	// instead of a locale->string object — exactly the shape
	// ResolveLocalized cannot resolve, the same class of gap uc-infra#174/
	// #199 already document for a different field type change.
	if _, err := db.Exec(`UPDATE records SET data = jsonb_set(data, '{name}', '"LegacyEach"') WHERE id = $1`, unit.Data.ID); err != nil {
		t.Fatalf("simulate legacy plain-string value: %v", err)
	}

	wRec := postForm(t, mux, "/api/records/Widget2", tenantID, "farshid", "sku=W-1&unit_id="+unit.Data.ID)
	if wRec.Code != http.StatusCreated {
		t.Fatalf("create widget: %d: %s", wRec.Code, wRec.Body.String())
	}

	list := getAs(t, mux, "/records/Widget2", tenantID, "farshid")
	if list.Code != http.StatusOK {
		t.Fatalf("list: %d: %s", list.Code, list.Body.String())
	}
	body := list.Body.String()
	if !strings.Contains(body, "LegacyEach") {
		t.Fatalf("expected the legacy plain-string label \"LegacyEach\" to render, got:\n%s", body)
	}
	if strings.Contains(body, unit.Data.ID) {
		t.Fatalf("legacy plain-string i18n_text value must not fall back to the raw record id:\n%s", body)
	}
}

// TestAPI_RecordLabel_I18nTextAllBlankObjectStillFallsBackToRawID pins
// PRESERVED (not new) behaviour, per independent review: a well-formed
// i18n_text object with no usable translation in ANY locale — as opposed
// to the legacy-plain-string case above, which is not a map at all —
// still falls back to the raw record id, not to the legacy-plain-string
// fallthrough this change added. ResolveLocalized returns ("", true) for
// an all-blank object (ok=true, unlike the plain-string case's (_, false)),
// so recordLabel's own `ok && s != ""` guard must keep rejecting it, and
// the subsequent rec.Data[labelField].(string) type assertion must keep
// failing (the stored value is a map, not a string) — the fix must not
// have accidentally widened either check.
func TestAPI_RecordLabel_I18nTextAllBlankObjectStillFallsBackToRawID(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, unitI18nEntityDef(), unitI18nFormDef())
	publishEntityAndForm(t, db, widgetRefEntityDef(), widgetRefFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	unitRec := postForm(t, mux, "/api/records/MultiUnit", tenantID, "farshid", "name.en=Each&name.tr=Adet")
	if unitRec.Code != http.StatusCreated {
		t.Fatalf("create unit: %d: %s", unitRec.Code, unitRec.Body.String())
	}
	var unit struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(unitRec.Body.Bytes(), &unit)

	// A well-formed i18n object where every locale is blank — legal shape
	// (ResolveLocalized's own contract, internal/i18n/i18n.go), just
	// nothing to show.
	if _, err := db.Exec(`UPDATE records SET data = jsonb_set(data, '{name}', '{"en": "", "tr": ""}'::jsonb) WHERE id = $1`, unit.Data.ID); err != nil {
		t.Fatalf("simulate all-blank i18n object: %v", err)
	}

	wRec := postForm(t, mux, "/api/records/Widget2", tenantID, "farshid", "sku=W-1&unit_id="+unit.Data.ID)
	if wRec.Code != http.StatusCreated {
		t.Fatalf("create widget: %d: %s", wRec.Code, wRec.Body.String())
	}

	list := getAs(t, mux, "/records/Widget2", tenantID, "farshid")
	if list.Code != http.StatusOK {
		t.Fatalf("list: %d: %s", list.Code, list.Body.String())
	}
	body := list.Body.String()
	if !strings.Contains(body, unit.Data.ID) {
		t.Fatalf("an all-blank i18n_text object should still fall back to the raw record id, got:\n%s", body)
	}
}
