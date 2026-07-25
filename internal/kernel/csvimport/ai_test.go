package csvimport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/aiassist"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

func purchaseOrderDefForAI() *entity.Definition {
	return &entity.Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Fields: []entity.Field{
			{Name: "po_number", Type: entity.FieldString, Required: true},
			{Name: "vendor_id", Type: entity.FieldString, Required: true},
			{Name: "order_date", Type: entity.FieldDate, Required: true},
			{Name: "total", Type: entity.FieldNumber},
		},
	}
}

func TestSampleRows_CapsAtMaxSampleRows(t *testing.T) {
	var b strings.Builder
	b.WriteString("po_number,total\n")
	for range maxSampleRowsForAI + 10 {
		b.WriteString("PO-1,100\n")
	}
	headers, rows, err := SampleRows(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("SampleRows: %v", err)
	}
	if len(headers) != 2 {
		t.Fatalf("expected 2 headers, got %+v", headers)
	}
	if len(rows) != maxSampleRowsForAI {
		t.Fatalf("expected exactly %d sample rows, got %d", maxSampleRowsForAI, len(rows))
	}
}

// aiServer builds an httptest server standing in for Ollama's
// /api/generate, replying with a fixed mappings response — the same
// wire shape aiassist.Client.GenerateJSON expects (see aiassist's own
// tests for the contract this mirrors).
func aiServer(t *testing.T, mappings []map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respJSON, err := json.Marshal(map[string]any{"mappings": mappings})
		if err != nil {
			t.Fatalf("marshal fake mappings: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": string(respJSON),
			"done":     true,
		})
	}))
}

func TestSuggestMappingAI_DisabledClientReturnsExistingUnchanged(t *testing.T) {
	existing := ColumnMapping{"PO No": "po_number"}
	// A typed nil, not a bare nil literal: SuggestMappingAI's ai parameter
	// is the aiprovider.Provider interface (not a concrete *aiassist.
	// Client) now that a caller may hand it any BYOK provider — a bare
	// nil there would be a true nil interface (no dynamic type at all),
	// and calling .Enabled() on that panics, unlike calling it on a
	// typed-nil pointer whose Enabled() has a nil-safe receiver. Every
	// real caller (internal/api's aiProviderFor) always returns a typed
	// value, this test just has to mirror that same discipline.
	var ai *aiassist.Client
	mapping, aiSuggested, err := SuggestMappingAI(context.Background(), ai,
		[]string{"PO No", "Vendor"}, nil, purchaseOrderDefForAI(), existing)
	if err != nil {
		t.Fatalf("expected no error from a disabled client, got %v", err)
	}
	if len(mapping) != 1 || mapping["PO No"] != "po_number" {
		t.Fatalf("expected mapping unchanged from existing, got %+v", mapping)
	}
	if len(aiSuggested) != 0 {
		t.Fatalf("expected no AI-suggested entries from a disabled client, got %+v", aiSuggested)
	}
}

func TestSuggestMappingAI_NoUnmappedHeadersSkipsTheCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	ai := aiassist.NewClient(srv.URL, "llama3.2:3b")

	existing := ColumnMapping{"PO No": "po_number"}
	_, aiSuggested, err := SuggestMappingAI(context.Background(), ai,
		[]string{"PO No"}, nil, purchaseOrderDefForAI(), existing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Fatal("expected no AI call when every header is already mapped")
	}
	if len(aiSuggested) != 0 {
		t.Fatalf("expected no AI-suggested entries, got %+v", aiSuggested)
	}
}

func TestSuggestMappingAI_FillsOnlyUnmappedHeaders(t *testing.T) {
	srv := aiServer(t, []map[string]string{
		{"column": "Vendor Nm", "database_field": "vendor_id"},
		{"column": "Dt", "database_field": "order_date"},
	})
	defer srv.Close()
	ai := aiassist.NewClient(srv.URL, "llama3.2:3b")

	headers := []string{"PO No", "Vendor Nm", "Dt"}
	sampleRows := [][]string{{"PO-1", "Acme", "2026-07-01"}}
	existing := ColumnMapping{"PO No": "po_number"} // exact match already resolved, not sent to AI

	mapping, aiSuggested, err := SuggestMappingAI(context.Background(), ai, headers, sampleRows, purchaseOrderDefForAI(), existing)
	if err != nil {
		t.Fatalf("SuggestMappingAI: %v", err)
	}
	want := ColumnMapping{"PO No": "po_number", "Vendor Nm": "vendor_id", "Dt": "order_date"}
	if len(mapping) != len(want) {
		t.Fatalf("expected %+v, got %+v", want, mapping)
	}
	for h, f := range want {
		if mapping[h] != f {
			t.Fatalf("expected %q -> %q, got %+v", h, f, mapping)
		}
	}
	if !aiSuggested["Vendor Nm"] || !aiSuggested["Dt"] {
		t.Fatalf("expected both AI-filled headers marked in aiSuggested, got %+v", aiSuggested)
	}
	if aiSuggested["PO No"] {
		t.Fatalf("expected the pre-existing exact match NOT marked as AI-suggested, got %+v", aiSuggested)
	}

	if err := ValidateMapping(purchaseOrderDefForAI(), headers, mapping); err != nil {
		t.Fatalf("expected the merged mapping to pass ValidateMapping, got %v", err)
	}
}

// TestSuggestMappingAI_IgnoresHallucinatedFieldName is the core safety
// property: a small model can propose a field name that doesn't exist
// on the Definition at all (or wasn't in the available/unclaimed set) —
// this must never make it into the returned mapping.
func TestSuggestMappingAI_IgnoresHallucinatedFieldName(t *testing.T) {
	srv := aiServer(t, []map[string]string{
		{"column": "Vendor Nm", "database_field": "supplier_name_that_does_not_exist"},
	})
	defer srv.Close()
	ai := aiassist.NewClient(srv.URL, "llama3.2:3b")

	mapping, aiSuggested, err := SuggestMappingAI(context.Background(), ai,
		[]string{"Vendor Nm"}, nil, purchaseOrderDefForAI(), ColumnMapping{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mapping) != 0 {
		t.Fatalf("expected the hallucinated field to be dropped, got %+v", mapping)
	}
	if len(aiSuggested) != 0 {
		t.Fatalf("expected no AI-suggested entries, got %+v", aiSuggested)
	}
}

// TestSuggestMappingAI_IgnoresHeaderNotInUnmappedSet guards the same
// kind of thing from the other direction: a response naming a header
// that either isn't in this file at all, or was already mapped, must
// never overwrite an existing mapping or fabricate a new column.
func TestSuggestMappingAI_IgnoresHeaderNotInUnmappedSet(t *testing.T) {
	srv := aiServer(t, []map[string]string{
		{"column": "PO No", "database_field": "vendor_id"}, // "PO No" already claimed by an exact match below
		{"column": "Not A Real Column", "database_field": "total"},
	})
	defer srv.Close()
	ai := aiassist.NewClient(srv.URL, "llama3.2:3b")

	existing := ColumnMapping{"PO No": "po_number"}
	mapping, aiSuggested, err := SuggestMappingAI(context.Background(), ai,
		[]string{"PO No", "Vendor"}, nil, purchaseOrderDefForAI(), existing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mapping["PO No"] != "po_number" {
		t.Fatalf("expected the existing mapping for %q left untouched, got %+v", "PO No", mapping)
	}
	if len(mapping) != 1 {
		t.Fatalf("expected no new entries accepted, got %+v", mapping)
	}
	if len(aiSuggested) != 0 {
		t.Fatalf("expected no AI-suggested entries, got %+v", aiSuggested)
	}
}

// TestSuggestMappingAI_DedupesFieldClaimedTwice mirrors SuggestMapping's
// own first-wins discipline for the AI path: if the model's response
// somehow claims the same target field for two different columns, only
// the first is accepted — never both (ValidateMapping would reject a
// mapping with two headers targeting one field outright).
func TestSuggestMappingAI_DedupesFieldClaimedTwice(t *testing.T) {
	srv := aiServer(t, []map[string]string{
		{"column": "Vendor Nm", "database_field": "vendor_id"},
		{"column": "Supplier", "database_field": "vendor_id"},
	})
	defer srv.Close()
	ai := aiassist.NewClient(srv.URL, "llama3.2:3b")

	mapping, aiSuggested, err := SuggestMappingAI(context.Background(), ai,
		[]string{"Vendor Nm", "Supplier"}, nil, purchaseOrderDefForAI(), ColumnMapping{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	claims := 0
	for _, f := range mapping {
		if f == "vendor_id" {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("expected exactly one header to claim vendor_id, got %d in %+v", claims, mapping)
	}
	if mapping["Vendor Nm"] != "vendor_id" || len(aiSuggested) != 1 {
		t.Fatalf("expected the first-occurring suggestion to win, got mapping=%+v aiSuggested=%+v", mapping, aiSuggested)
	}
}

// TestSuggestMappingAI_DedupesColumnClaimedTwice is
// TestSuggestMappingAI_DedupesFieldClaimedTwice's counterpart from the
// other direction: if the model's response lists the same column twice
// (with different field answers), only the first is accepted — the
// second must never silently override it.
func TestSuggestMappingAI_DedupesColumnClaimedTwice(t *testing.T) {
	srv := aiServer(t, []map[string]string{
		{"column": "Vendor Nm", "database_field": "vendor_id"},
		{"column": "Vendor Nm", "database_field": "po_number"},
	})
	defer srv.Close()
	ai := aiassist.NewClient(srv.URL, "llama3.2:3b")

	mapping, aiSuggested, err := SuggestMappingAI(context.Background(), ai,
		[]string{"Vendor Nm"}, nil, purchaseOrderDefForAI(), ColumnMapping{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mapping["Vendor Nm"] != "vendor_id" {
		t.Fatalf("expected the first-occurring answer for the repeated column to win, got %+v", mapping)
	}
	if len(mapping) != 1 || len(aiSuggested) != 1 {
		t.Fatalf("expected exactly one accepted mapping, got mapping=%+v aiSuggested=%+v", mapping, aiSuggested)
	}
}

func TestSuggestMappingAI_ServerErrorReturnsExistingWithAdvisoryErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	ai := aiassist.NewClient(srv.URL, "llama3.2:3b")

	existing := ColumnMapping{"PO No": "po_number"}
	mapping, aiSuggested, err := SuggestMappingAI(context.Background(), ai,
		[]string{"PO No", "Vendor"}, nil, purchaseOrderDefForAI(), existing)
	if err == nil {
		t.Fatal("expected an advisory error when the AI server fails")
	}
	if len(mapping) != 1 || mapping["PO No"] != "po_number" {
		t.Fatalf("expected the existing mapping unchanged despite the AI failure, got %+v", mapping)
	}
	if len(aiSuggested) != 0 {
		t.Fatalf("expected no AI-suggested entries on failure, got %+v", aiSuggested)
	}
}
