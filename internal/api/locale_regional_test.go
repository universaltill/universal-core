package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// regionalEntityDef is a minimal entity with one date and one number
// field — the two types regional formatting actually changes.
func regionalEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Shipment", Version: 1, Module: "foundation",
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "ship_date", Type: entity.FieldDate},
			{Name: "weight", Type: entity.FieldNumber},
		},
	}
}

func setupRegionalTenant(t *testing.T) (tenantID string, mux *http.ServeMux) {
	t.Helper()
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, regionalEntityDef(), &form.Definition{
		EntityType: "Shipment", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}, {Name: "ship_date"}, {Name: "weight"}}}},
	})
	if _, err := crud.NewEngine(db).Create(ctx, regionalEntityDef(), map[string]any{
		"name": "Container 1", "ship_date": "2026-04-03", "weight": 1234567.5,
	}, humanActor()); err != nil {
		t.Fatalf("seed Shipment: %v", err)
	}
	mux = http.NewServeMux()
	testHandler(t, router).Routes(mux)
	return tenantID, mux
}

// setupRegionalTenantTwoRows is setupRegionalTenant plus a second
// Shipment record whose weight doesn't match the first row's under any
// region this suite exercises — so "the full list, unfiltered" and "the
// original filtered result" are actually distinguishable, not the same
// one-row page either way (uc-infra#128's independent review: a
// single-row fixture can't prove a filter was truly CLEARED rather than
// still active and coincidentally still matching that one row).
func setupRegionalTenantTwoRows(t *testing.T) (tenantID string, mux *http.ServeMux) {
	t.Helper()
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, regionalEntityDef(), &form.Definition{
		EntityType: "Shipment", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}, {Name: "ship_date"}, {Name: "weight"}}}},
	})
	eng := crud.NewEngine(db)
	if _, err := eng.Create(ctx, regionalEntityDef(), map[string]any{
		"name": "Container 1", "ship_date": "2026-04-03", "weight": 1234567.5,
	}, humanActor()); err != nil {
		t.Fatalf("seed Shipment: %v", err)
	}
	if _, err := eng.Create(ctx, regionalEntityDef(), map[string]any{
		"name": "Container 2", "ship_date": "2026-01-15", "weight": 42.0,
	}, humanActor()); err != nil {
		t.Fatalf("seed second Shipment: %v", err)
	}
	mux = http.NewServeMux()
	testHandler(t, router).Routes(mux)
	return tenantID, mux
}

// setupAmbiguousDateTenant seeds two Shipment rows whose ISO dates are
// each other's day/month swap — 3 April vs 4 March — so the SAME literal
// date-shaped text ("03/04/2026") is a valid, DIFFERENT match under
// en-GB's day-first rules than under en-US's month-first rules. That's
// what makes it possible to prove a region switch REINTERPRETS a filter
// (matches a different record, silently) rather than merely stopping it
// from matching anything — the weight-grouping pair used elsewhere in
// this file only demonstrates the latter, weaker failure mode
// (uc-infra#128's independent review, F1).
func setupAmbiguousDateTenant(t *testing.T) (tenantID string, mux *http.ServeMux) {
	t.Helper()
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, regionalEntityDef(), &form.Definition{
		EntityType: "Shipment", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}, {Name: "ship_date"}, {Name: "weight"}}}},
	})
	eng := crud.NewEngine(db)
	if _, err := eng.Create(ctx, regionalEntityDef(), map[string]any{
		"name": "AprilThird", "ship_date": "2026-04-03", "weight": 1.0,
	}, humanActor()); err != nil {
		t.Fatalf("seed AprilThird: %v", err)
	}
	if _, err := eng.Create(ctx, regionalEntityDef(), map[string]any{
		"name": "MarchFourth", "ship_date": "2026-03-04", "weight": 2.0,
	}, humanActor()); err != nil {
		t.Fatalf("seed MarchFourth: %v", err)
	}
	mux = http.NewServeMux()
	testHandler(t, router).Routes(mux)
	return tenantID, mux
}

func getList(t *testing.T, mux *http.ServeMux, tenantID, query string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest("GET", "/records/Shipment"+query, tenantID, "farshid", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /records/Shipment%s: expected 200, got %d: %s", query, rec.Code, rec.Body.String())
	}
	return rec
}

// TestListPage_RegionalDateAndNumberFormatting is the card's headline
// behaviour at the HTTP layer: the same stored record renders
// differently per region, and the ISO/plain-decimal storage form never
// appears in the cell.
func TestListPage_RegionalDateAndNumberFormatting(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	for name, tc := range map[string]struct {
		query    string
		wantDate string
		wantNum  string
	}{
		"british default":    {"", "03/04/2026", "1,234,567.5"},
		"american":           {"?region=en-US", "04/03/2026", "1,234,567.5"},
		"turkish":            {"?lang=tr&region=tr-TR", "03.04.2026", "1.234.567,5"},
		"farsi jalali":       {"?lang=fa&region=fa-IR", "۱۴۰۵/۰۱/۱۴", "۱٬۲۳۴٬۵۶۷٫۵"},
		"farsi gregorian":    {"?lang=fa&region=fa-IR-u-ca-gregory", "۲۰۲۶/۰۴/۰۳", "۱٬۲۳۴٬۵۶۷٫۵"},
		"arabic gulf digits": {"?lang=ar&region=ar-AE", "٠٣/٠٤/٢٠٢٦", "١٬٢٣٤٬٥٦٧٫٥"},
	} {
		t.Run(name, func(t *testing.T) {
			body := getList(t, mux, tenantID, tc.query).Body.String()
			if !strings.Contains(body, tc.wantDate) {
				t.Errorf("list page missing regional date %q\n%s", tc.wantDate, excerpt(body))
			}
			if !strings.Contains(body, tc.wantNum) {
				t.Errorf("list page missing regional number %q\n%s", tc.wantNum, excerpt(body))
			}
			// The raw stored forms must not leak into the rendered cell
			// for a non-default region — that would mean the formatter
			// never ran.
			if tc.wantDate != "2026-04-03" && strings.Contains(body, ">2026-04-03<") {
				t.Errorf("raw ISO date leaked into a cell for %s", name)
			}
		})
	}
}

// regionalParentEntityDef/regionalChildEntityDef are a dedicated
// master-detail fixture for TestMasterDetailChildCells_
// RegionalDateAndNumberFormatting below, rather than extending
// purchaseOrderEntityDef/poLineEntityDef (handlers_test.go): those are
// shared by several other tests already, and this file's own
// regionalEntityDef (used by the top-level list tests above) has no
// composition relationship to hang a child section off of. Same "own
// fixture, not a shared one" reasoning handlers_test.go's own
// itemEntityDef/poLineWithItemRefEntityDef comment documents.
func regionalParentEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Shipment", Version: 1, Module: "foundation",
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
		},
		Relationships: []entity.Relationship{
			{Name: "legs", Kind: entity.RelationComposition, Target: "ShipmentLeg", ParentField: "shipment_id"},
		},
	}
}

func regionalParentFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "Shipment", Version: 1,
		Sections: []form.Section{
			{Title: "Header", Component: form.ComponentFields, Fields: []form.FormField{{Name: "name", Label: "Name"}}},
			{Title: "Legs", Component: form.ComponentMasterDetail, Target: "ShipmentLeg"},
		},
	}
}

func regionalChildEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "ShipmentLeg", Version: 1,
		Fields: []entity.Field{
			{Name: "shipment_id", Type: entity.FieldString, Required: true},
			{Name: "depart_date", Type: entity.FieldDate},
			{Name: "distance_km", Type: entity.FieldNumber},
		},
	}
}

func regionalChildFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "ShipmentLeg", Version: 1,
		Sections: []form.Section{{
			Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "depart_date", Label: "Depart Date"}, {Name: "distance_km", Label: "Distance"}},
		}},
	}
}

// TestMasterDetailChildCells_RegionalDateAndNumberFormatting (uc-infra
// #133) is the internal/api-layer counterpart to
// TestListPage_RegionalDateAndNumberFormatting above, but for a
// master_detail child cell instead of a top-level list column: the same
// stored record, the same regional query params, against real Postgres
// and the real form-rendering HTTP path (loadMasterDetailChildren →
// buildFormRenderData → formrender.Render), not a synthetic
// formrender.Data handed to the renderer directly the way
// formrender's own unit tests exercise this (see
// TestRender_MasterDetailDateAndNumberCellsAreRegionallyFormatted for
// that half). Same worked date/number pair and expected outputs as the
// sibling list-page test, so a mismatch here would mean the two render
// paths actually disagree.
func TestMasterDetailChildCells_RegionalDateAndNumberFormatting(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, regionalParentEntityDef(), regionalParentFormDef())
	publishEntityAndForm(t, db, regionalChildEntityDef(), regionalChildFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	shipReq := newRequest("POST", "/api/records/Shipment", tenantID, "farshid", []byte(`{"name":"Container 1"}`))
	shipRec := httptest.NewRecorder()
	mux.ServeHTTP(shipRec, shipReq)
	var ship struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(shipRec.Body.Bytes(), &ship); err != nil {
		t.Fatalf("unmarshal Shipment: %v", err)
	}

	legBody := []byte(`{"shipment_id":"` + ship.Data.ID + `","depart_date":"2026-04-03","distance_km":1234567.5}`)
	legReq := newRequest("POST", "/api/records/ShipmentLeg", tenantID, "farshid", legBody)
	legRec := httptest.NewRecorder()
	mux.ServeHTTP(legRec, legReq)
	if legRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating ShipmentLeg, got %d: %s", legRec.Code, legRec.Body.String())
	}

	for name, tc := range map[string]struct {
		query    string
		wantDate string
		wantNum  string
	}{
		"british default": {"", "03/04/2026", "1,234,567.5"},
		"american":        {"?region=en-US", "04/03/2026", "1,234,567.5"},
		"turkish":         {"?lang=tr&region=tr-TR", "03.04.2026", "1.234.567,5"},
		"farsi jalali":    {"?lang=fa&region=fa-IR", "۱۴۰۵/۰۱/۱۴", "۱٬۲۳۴٬۵۶۷٫۵"},
	} {
		t.Run(name, func(t *testing.T) {
			req := newRequest("GET", "/forms/Shipment/"+ship.Data.ID+tc.query, tenantID, "farshid", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /forms/Shipment/%s%s: expected 200, got %d: %s", ship.Data.ID, tc.query, rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, `<td data-field="depart_date">`+tc.wantDate+`</td>`) {
				t.Errorf("expected regionally formatted date %q in the depart_date child cell\n%s", tc.wantDate, excerpt(body))
			}
			if !strings.Contains(body, `<td data-field="distance_km">`+tc.wantNum+`</td>`) {
				t.Errorf("expected regionally formatted number %q in the distance_km child cell\n%s", tc.wantNum, excerpt(body))
			}
			if strings.Contains(body, `>2026-04-03<`) {
				t.Errorf("raw ISO date leaked into a %s-locale child cell\n%s", name, excerpt(body))
			}
		})
	}
}

// An explicit ?region= is persisted, so the next plain request keeps
// formatting the same way — the cookie half of the preference.
func TestRegionPreference_PersistsInCookie(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	rec := getList(t, mux, tenantID, "?region=en-US")
	var regionCk *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == regionCookie {
			regionCk = c
		}
	}
	if regionCk == nil {
		t.Fatal("expected an explicit ?region= to be persisted in a cookie")
	}
	if regionCk.Value != "en-US" {
		t.Fatalf("cookie = %q, want the canonical tag en-US", regionCk.Value)
	}

	body := getList(t, mux, tenantID, "", regionCk).Body.String()
	if !strings.Contains(body, "04/03/2026") {
		t.Errorf("persisted region did not survive to the next request\n%s", excerpt(body))
	}
}

// An invalid or foreign-language preference is ignored, never persisted
// and never surfaced as an error — same contract as an unknown ?lang=.
func TestRegionPreference_InvalidIgnored(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	for name, query := range map[string]string{
		"unknown region":   "?region=en-ZZ",
		"garbage":          "?region=not-a-locale",
		"unsupported lang": "?region=de-DE",
		// Valid tag, but for a language the page isn't being rendered
		// in: switching the UI to English must not keep Turkish
		// formatting.
		"foreign language": "?region=tr-TR",
	} {
		t.Run(name, func(t *testing.T) {
			rec := getList(t, mux, tenantID, query)
			for _, c := range rec.Result().Cookies() {
				if c.Name == regionCookie {
					t.Errorf("invalid preference %q must not be persisted (got %q)", query, c.Value)
				}
			}
			if !strings.Contains(rec.Body.String(), "03/04/2026") {
				t.Errorf("expected the en-GB default formatting for %q\n%s", query, excerpt(rec.Body.String()))
			}
		})
	}
}

// A stored region belonging to another language is dropped when the UI
// language changes — otherwise switching to Turkish would keep writing
// American dates.
func TestRegionPreference_DroppedOnLanguageSwitch(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)
	usCookie := &http.Cookie{Name: regionCookie, Value: "en-US"}

	body := getList(t, mux, tenantID, "?lang=tr", usCookie).Body.String()
	if !strings.Contains(body, "03.04.2026") {
		t.Errorf("expected Turkish default formatting after the language switch\n%s", excerpt(body))
	}
	if strings.Contains(body, "04/03/2026") {
		t.Error("the en-US preference survived a switch to Turkish")
	}
}

// The picker has to exist for any of this to be reachable, and it must
// offer the active language's regions with the current one selected.
func TestNav_RegionPickerRendered(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	body := getList(t, mux, tenantID, "?region=en-US").Body.String()
	if !strings.Contains(body, `class="uc-nav-region"`) {
		t.Fatalf("nav is missing the region picker\n%s", excerpt(body))
	}
	if !strings.Contains(body, "?region=en-GB") || !strings.Contains(body, "?region=en-US") {
		t.Errorf("region picker is missing the English choices\n%s", excerpt(body))
	}
	if !strings.Contains(body, `value="/records/Shipment?region=en-US" selected`) {
		t.Errorf("region picker does not mark the active region selected\n%s", excerpt(body))
	}
	// Turkish has a single region, so the picker is suppressed rather
	// than rendering a one-option dropdown.
	tr := getList(t, mux, tenantID, "?lang=tr").Body.String()
	if strings.Contains(tr, `class="uc-nav-region"`) {
		t.Error("a single-region language should not render a picker")
	}
}

// Form inputs keep the raw ISO/plain values — they round-trip back
// through csvimport.Coerce on submit, so a localized value there would
// either fail to parse or parse as the wrong date. This is the
// scope boundary the locale package documents, asserted.
func TestFormInputsStayUnlocalized(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	// Find the seeded record's form.
	list := getList(t, mux, tenantID, "?lang=fa&region=fa-IR").Body.String()
	id := recordIDFromListPage(t, list)

	req := newRequest("GET", "/forms/Shipment/"+id+"?lang=fa&region=fa-IR", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET form: %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="2026-04-03"`) {
		t.Errorf("date input must carry the raw ISO value for round-tripping\n%s", excerpt(body))
	}
	if strings.Contains(body, `value="۱۴۰۵/۰۱/۱۴"`) {
		t.Error("a localized date reached a form input — it would not round-trip on submit")
	}
	if !strings.Contains(body, `value="1234567.5"`) {
		t.Errorf("number input must carry the raw decimal value\n%s", excerpt(body))
	}
}

func recordIDFromListPage(t *testing.T, body string) string {
	t.Helper()
	const marker = `/forms/Shipment/`
	// Skip the "New" link, which uses the same prefix with the literal
	// id "new" and appears before any row.
	for rest := body; ; {
		i := strings.Index(rest, marker)
		if i < 0 {
			t.Fatalf("no record link on the list page\n%s", excerpt(body))
		}
		rest = rest[i+len(marker):]
		end := strings.IndexAny(rest, `"?`)
		if end < 0 {
			t.Fatalf("malformed record link")
		}
		if id := rest[:end]; id != "new" && id != "" {
			return id
		}
	}
}

func excerpt(s string) string {
	if len(s) > 1500 {
		return s[:1500] + "…"
	}
	return s
}

// TestRegionPreference_PersistsFromAnyPage is the regression test for
// the independent review's second blocker: the picker is rendered in
// the nav on every page, but only the record-list handler persisted a
// choice. Picking a region on the dashboard appeared to work (the
// reloaded page showed it selected, because the query param is honoured
// for that one request) and then silently reverted everywhere.
func TestRegionPreference_PersistsFromAnyPage(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	for _, path := range []string{"/", "/forms/Shipment/new", "/issue-report/new"} {
		t.Run(path, func(t *testing.T) {
			req := newRequest("GET", path+"?region=en-US", tenantID, "farshid", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s: %d", path, rec.Code)
			}
			var ck *http.Cookie
			for _, c := range rec.Result().Cookies() {
				if c.Name == regionCookie {
					ck = c
				}
			}
			if ck == nil || ck.Value != "en-US" {
				t.Fatalf("choosing a region on %s did not persist it (cookie %+v)", path, ck)
			}
			// And it actually takes effect on the page that formats.
			body := getList(t, mux, tenantID, "", ck).Body.String()
			if !strings.Contains(body, "04/03/2026") {
				t.Errorf("the preference chosen on %s did not reach the list page\n%s", path, excerpt(body))
			}
		})
	}
}

// TestRegionPreference_OnlyOfferedChoicesAccepted: locale.Parse accepts
// any language/region/calendar crossing, so the API layer restricts to
// what the picker actually offers. Without this, ?region=tr-US pinned a
// Turkish user to American date order permanently — Turkish has one
// region, so the picker is suppressed and there was no way to undo it.
func TestRegionPreference_OnlyOfferedChoicesAccepted(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	for name, tc := range map[string]struct{ query, wantDate string }{
		"calendar crossing":  {"?region=en-GB-u-ca-jalali", "03/04/2026"},
		"digits crossing":    {"?region=en-AE", "03/04/2026"},
		"region crossing":    {"?region=en-IR", "03/04/2026"},
		"single-region lang": {"?lang=tr&region=tr-US", "03.04.2026"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := getList(t, mux, tenantID, tc.query)
			for _, c := range rec.Result().Cookies() {
				if c.Name == regionCookie {
					t.Errorf("%s must not be persisted, got cookie %q", tc.query, c.Value)
				}
			}
			if !strings.Contains(rec.Body.String(), tc.wantDate) {
				t.Errorf("expected the language's default formatting %q for %s\n%s",
					tc.wantDate, tc.query, excerpt(rec.Body.String()))
			}
		})
	}
}

// The picker's labels are translated and its links preserve the current
// query string — picking a region on a filtered, sorted, paginated list
// must not silently reset all three.
func TestNav_RegionPickerLabelsAndQueryPreservation(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	body := getList(t, mux, tenantID, "?sort=name&dir=desc&q=Container&filter=name").Body.String()
	if !strings.Contains(body, ">United States<") {
		t.Errorf("region option is not translated (expected a real place name)\n%s", excerpt(body))
	}
	if strings.Contains(body, ">en-US<") {
		t.Error("a raw BCP 47 tag is being shown to the user as an option label")
	}
	if !strings.Contains(body, "dir=desc") || !strings.Contains(body, "q=Container") {
		t.Errorf("region links dropped the active sort/filter\n%s", excerpt(body))
	}

	// Farsi's Gregorian variant must read as a place + calendar, never
	// as the -u-ca- extension subtag.
	fa := getList(t, mux, tenantID, "?lang=fa").Body.String()
	if strings.Contains(fa, ">fa-IR-u-ca-gregory<") {
		t.Error("the BCP 47 extension subtag is being shown to the user")
	}
}
