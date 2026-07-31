package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	// JSON object); the plain `code` column still is.
	if strings.Contains(body, `href="/records/LocalizedThing?sort=name`) {
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

// TestAPI_I18nText_SearchDegradesSortFilter confirms the search endpoint
// does not 500 or error when the label field is i18n_text — it can't
// sort/filter by a JSON object (ADR-0009 deferral), so it returns a capped
// unsorted list rather than failing. A ?q= is accepted but simply doesn't
// filter this slice.
func TestAPI_I18nText_SearchDegradesSortFilter(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, unitI18nEntityDef(), unitI18nFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	postForm(t, mux, "/api/records/MultiUnit", tenantID, "farshid", "name.en=Each&name.tr=Adet")
	postForm(t, mux, "/api/records/MultiUnit", tenantID, "farshid", "name.en=Box&name.tr=Kutu")

	// A query that would filter a plain-string label: here it must still
	// return 200 with results (unfiltered), never a 500 from sorting/
	// filtering by a JSON object.
	rec := getAs(t, mux, "/api/references/MultiUnit?q=Each&lang=en", tenantID, "farshid")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (degraded, not errored), got %d: %s", rec.Code, rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 2 {
		t.Fatalf("expected both units back (filter degraded to none), got %d: %+v", len(opts), opts)
	}
}
