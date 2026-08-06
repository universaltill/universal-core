package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// moneyEntityDef is a minimal entity with one FieldMoney field —
// RequestForQuotationQuoteLine's own shape (uc-infra#68), without
// coupling this integration test to the purchasing module.
func moneyEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "QuoteLine", Version: 1, Module: "foundation",
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "unit_price", Type: entity.FieldMoney},
		},
	}
}

// TestListPage_MoneyFieldDisplaysMajorUnitDecimal is this card's
// headline integration behaviour: a record stored with unit_price as
// 1050 (minor units) must render its list cell as the regionally
// formatted major-unit amount ("1,234,567.89" default region), never the
// raw stored integer — proving the fix reaches real HTTP responses
// against a real Postgres record, not just the unit-level formatter.
func TestListPage_MoneyFieldDisplaysMajorUnitDecimal(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, moneyEntityDef(), &form.Definition{
		EntityType: "QuoteLine", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}, {Name: "unit_price"}}}},
	})
	// 123456789 minor units == $1,234,567.89 — large enough that a naive
	// float64 accumulation elsewhere in this stack would already show an
	// IEEE artifact if one were reintroduced, and distinct enough from
	// its own raw integer form to catch a missing FieldMoney case
	// falling through to the generic float formatter.
	if _, err := crud.NewEngine(db).Create(ctx, moneyEntityDef(), map[string]any{
		"name": "Line 1", "unit_price": float64(123456789),
	}, humanActor()); err != nil {
		t.Fatalf("seed QuoteLine: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	req := newRequest("GET", "/records/QuoteLine", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /records/QuoteLine: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "1,234,567.89") {
		t.Fatalf("expected the list cell to show 1,234,567.89, got:\n%s", excerpt(body))
	}
	if strings.Contains(body, ">123456789<") {
		t.Fatalf("raw minor-units integer leaked into the list cell:\n%s", excerpt(body))
	}
}

// TestListPage_MoneyFieldSortsNumerically confirms sorting by a money
// column uses SortNumeric (100 before 90 is wrong for text sort), the
// same fix already applied to FieldNumber columns.
func TestListPage_MoneyFieldSortsNumerically(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, moneyEntityDef(), &form.Definition{
		EntityType: "QuoteLine", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name"}, {Name: "unit_price"}}}},
	})
	eng := crud.NewEngine(db)
	for _, rec := range []map[string]any{
		{"name": "Ninety", "unit_price": float64(9000)},
		{"name": "OneHundred", "unit_price": float64(10000)},
	} {
		if _, err := eng.Create(ctx, moneyEntityDef(), rec, humanActor()); err != nil {
			t.Fatalf("seed QuoteLine %v: %v", rec, err)
		}
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	req := newRequest("GET", "/records/QuoteLine?sort=unit_price", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /records/QuoteLine?sort=unit_price: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	ninety := strings.Index(body, "Ninety")
	hundred := strings.Index(body, "OneHundred")
	if ninety == -1 || hundred == -1 {
		t.Fatalf("expected both rows present, got:\n%s", excerpt(body))
	}
	if ninety > hundred {
		t.Fatalf("expected 90 before 100 under numeric sort, got:\n%s", excerpt(body))
	}
}
