package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
)

// createVendor is a small helper: POST one Vendor record through the real
// handler and return its id. Used to seed enough records that the cap and
// the type-ahead filter are actually exercised, not asserted against a
// handful.
func createVendor(t *testing.T, mux *http.ServeMux, tenantID, name string) string {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q}`, name)
	req := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create vendor %q: expected 201, got %d: %s", name, rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	return created.Data.ID
}

// decodeRefOptions decodes the reference-search endpoint's JSON body into
// the {id,label} shape the combobox consumes.
func decodeRefOptions(t *testing.T, body []byte) []struct {
	ID    string `json:"id"`
	Label string `json:"label"`
} {
	t.Helper()
	var env struct {
		Data []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"data"`
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal reference options: %v (body=%s)", err, string(body))
	}
	if env.Error != nil {
		t.Fatalf("expected no error in envelope, got %q", *env.Error)
	}
	return env.Data
}

// TestReferenceSearch_ReturnsIDAndLabelForTargetEntity is the happy path:
// the endpoint returns [{id,label}] for the target entity, labelled by the
// `name` field, so the combobox has something to render.
func TestReferenceSearch_ReturnsIDAndLabelForTargetEntity(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	id := createVendor(t, mux, tenantID, "Acme Textiles")

	req := newRequest("GET", "/api/references/Vendor", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 {
		t.Fatalf("expected exactly one option, got %d: %+v", len(opts), opts)
	}
	if opts[0].ID != id || opts[0].Label != "Acme Textiles" {
		t.Fatalf("expected {%s, Acme Textiles}, got %+v", id, opts[0])
	}
}

// TestReferenceSearch_FiltersByTypeAhead confirms the ?q= type-ahead
// narrows by the label field — the whole reason the endpoint exists
// instead of shipping every record to the browser.
func TestReferenceSearch_FiltersByTypeAhead(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createVendor(t, mux, tenantID, "Acme Textiles")
	createVendor(t, mux, tenantID, "Beta Supplies")
	createVendor(t, mux, tenantID, "Acme Hardware")

	req := newRequest("GET", "/api/references/Vendor?q=acme", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 2 {
		t.Fatalf("expected the two Acme vendors, got %d: %+v", len(opts), opts)
	}
	for _, o := range opts {
		if o.Label != "Acme Textiles" && o.Label != "Acme Hardware" {
			t.Fatalf("type-ahead leaked a non-matching option: %+v", o)
		}
	}
}

// TestReferenceSearch_CapsResults proves the endpoint never returns more
// than referenceSearchLimit in one page, and that a second page continues
// where the first left off — the payload guarantee the picker relies on.
func TestReferenceSearch_CapsResults(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// One more than a full page, so page 2 has exactly the remainder.
	total := referenceSearchLimit + 5
	for i := 0; i < total; i++ {
		createVendor(t, mux, tenantID, fmt.Sprintf("Vendor %03d", i))
	}

	req := newRequest("GET", "/api/references/Vendor", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page 1: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	page1 := decodeRefOptions(t, rec.Body.Bytes())
	if len(page1) != referenceSearchLimit {
		t.Fatalf("page 1 should be capped at %d, got %d", referenceSearchLimit, len(page1))
	}

	req2 := newRequest("GET", "/api/references/Vendor?page=2", tenantID, "farshid", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("page 2: expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	page2 := decodeRefOptions(t, rec2.Body.Bytes())
	if len(page2) != total-referenceSearchLimit {
		t.Fatalf("page 2 should have the remainder %d, got %d", total-referenceSearchLimit, len(page2))
	}
	// The two pages must not overlap — a broken offset would repeat rows.
	seen := map[string]bool{}
	for _, o := range page1 {
		seen[o.ID] = true
	}
	for _, o := range page2 {
		if seen[o.ID] {
			t.Fatalf("page 2 repeated a page-1 id %s — offset is wrong", o.ID)
		}
	}
}

// TestReferenceSearch_UnknownEntityTypeIsNotFound confirms a bogus target
// is a clean lookup error, not a 500 or an empty 200 that would hide a
// mistyped Definition.
func TestReferenceSearch_UnknownEntityTypeIsNotFound(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/api/references/NoSuchEntity", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected a non-200 for an unknown entity type, got 200: %s", rec.Body.String())
	}
}

// TestReferenceSearch_HiddenLabelFieldDegradesNot403 is the regression
// test for the review finding that a FieldPermission hiding the label
// field (name/title) from a role used to 403 the ENTIRE picker for that
// role: the endpoint sorted/filtered by the label field unconditionally,
// and the guarded engine's rejectHiddenSortFilter then refused the whole
// request. A role that can legitimately read the entity but not its name
// field must still get a usable (id-labelled) picker, degrading the same
// way the form's own current-value label resolution does — never failing.
func TestReferenceSearch_HiddenLabelFieldDegradesNot403(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	// Hide Vendor.name from holders of the "clerk" role (user "user-clerk").
	seedFieldRule(t, db, "clerk", "user-clerk", "Vendor", "name")

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Create the vendor as an unrestricted user so the name is actually set.
	id := createVendor(t, mux, tenantID, "Acme Textiles")

	// The clerk searches: must be 200 (not 403), must return the record
	// labelled by its raw id (the hidden name is redacted), and must not
	// leak the hidden name value.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest("GET", "/api/references/Vendor", tenantID, "user-clerk", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("clerk with hidden label field should still get 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Acme Textiles") {
		t.Fatalf("the hidden name value must not leak into the picker, got: %s", rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].ID != id || opts[0].Label != id {
		t.Fatalf("expected exactly one option labelled by raw id %q, got %+v", id, opts)
	}
}

// createGroupedItem is createVendor's counterpart for groupedItemEntityDef
// (handlers_test.go) — a small throwaway self-referencing entity carrying
// MustMatchParentField (uc-infra#78), used by the source_entity_type/
// source_field/sibling_value tests below.
func createGroupedItem(t *testing.T, mux *http.ServeMux, tenantID, name, groupID string) string {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"group_id":%q}`, name, groupID)
	req := newRequest("POST", "/api/records/GroupedItem", tenantID, "farshid", []byte(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create GroupedItem %q: expected 201, got %d: %s", name, rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	return created.Data.ID
}

// TestReferenceSearch_SourceField_MustMatchParentFieldNarrowsBySiblingValue
// is the HTTP-level proof of MustMatchParentField's own narrowing
// (internal/kernel/crud/target_constraints_test.go's
// TestEngine_ResolveReferenceFilter_MustMatchParentField proves the
// mechanism itself) — independent review finding #7 flagged this had
// zero coverage at the handler layer. sibling_value is the field being
// tested for injection safety too: it flows straight from the query
// string into an EqualsFilters value (crud.Engine.ResolveReferenceFilter),
// so this also exercises that it narrows correctly rather than, say,
// matching everything.
func TestReferenceSearch_SourceField_MustMatchParentFieldNarrowsBySiblingValue(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, groupedItemEntityDef(), groupedItemFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	inGroupA := createGroupedItem(t, mux, tenantID, "A-item", "group-a")
	createGroupedItem(t, mux, tenantID, "B-item", "group-b")

	req := newRequest("GET",
		"/api/references/GroupedItem?source_entity_type=GroupedItem&source_field=parent_item_id&sibling_value=group-a",
		tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].ID != inGroupA {
		t.Fatalf("expected exactly the group-a item, got %+v", opts)
	}
}

// TestReferenceSearch_SourceField_UnresolvableSourceEntityTypeIgnored:
// a source_entity_type that doesn't resolve to a real Definition (a stale
// form, a typo) must degrade to an unnarrowed listing, not fail the whole
// search — the same graceful-degradation posture the label/sort logic
// already takes (independent review finding #7).
func TestReferenceSearch_SourceField_UnresolvableSourceEntityTypeIgnored(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	id := createVendor(t, mux, tenantID, "Acme Textiles")

	req := newRequest("GET",
		"/api/references/Vendor?source_entity_type=NoSuchEntity&source_field=whatever&sibling_value=x",
		tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite an unresolvable source_entity_type, got %d: %s", rec.Code, rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].ID != id {
		t.Fatalf("expected the unnarrowed vendor list, got %+v", opts)
	}
}

// TestReferenceSearch_SourceField_NonReferenceSourceFieldIgnored: a
// source_field that resolves but isn't a FieldReference (a stale form
// pointing at a plain string field) must also degrade to an unnarrowed
// listing rather than erroring or silently matching nothing.
func TestReferenceSearch_SourceField_NonReferenceSourceFieldIgnored(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, groupedItemEntityDef(), groupedItemFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	id := createGroupedItem(t, mux, tenantID, "A-item", "group-a")

	// "name" is a plain string field on GroupedItem, not a FieldReference.
	req := newRequest("GET",
		"/api/references/GroupedItem?source_entity_type=GroupedItem&source_field=name&sibling_value=group-a",
		tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite a non-reference source_field, got %d: %s", rec.Code, rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].ID != id {
		t.Fatalf("expected the unnarrowed list, got %+v", opts)
	}
}

// TestReferenceSearch_SourceField_MismatchedTargetIgnored: source_field
// IS a real FieldReference, but its declared Target is a DIFFERENT
// entity type than the one being searched (entityType) — a stale form
// pointing the picker at the wrong field entirely. Must degrade to an
// unnarrowed listing, not apply a constraint that belongs to a different
// target type.
func TestReferenceSearch_SourceField_MismatchedTargetIgnored(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityAndForm(t, db, groupedItemEntityDef(), groupedItemFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	id := createVendor(t, mux, tenantID, "Acme Textiles")

	// parent_item_id targets GroupedItem, not Vendor — searching Vendor
	// with it named as the source field must not apply its narrowing.
	req := newRequest("GET",
		"/api/references/Vendor?source_entity_type=GroupedItem&source_field=parent_item_id&sibling_value=group-a",
		tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite a mismatched-target source_field, got %d: %s", rec.Code, rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].ID != id {
		t.Fatalf("expected the unnarrowed vendor list, got %+v", opts)
	}
}

// TestReferenceSearch_RequiresAuth confirms the endpoint is behind the
// same auth gate as every other /api route — an unauthenticated request
// never reaches the data layer.
func TestReferenceSearch_RequiresAuth(t *testing.T) {
	router := newTestRouter(t)
	// Deliberately NOT enabling dev auth, and sending no tenant/actor.
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/api/references/Vendor", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected an auth failure, got 200: %s", rec.Body.String())
	}
}

// TestReferenceSearch_SourceField_StatusIDAutoScopedToOwnStatusType is
// the HTTP-level proof of ADR-0032's status_id auto-scoping
// (internal/kernel/crud/target_constraints_test.go's
// TestEngine_ResolveReferenceFilter_StatusIDScopedToOwnStatusType proves
// the mechanism itself) — against two REAL modules' real published
// Definitions (purchasing.PurchaseOrder, sales.SalesOrder), not a
// throwaway test Definition, so this also confirms the real
// entity_definitions rows these ship with actually carry StatusTypeCode
// as the mechanism assumes, and that no source_entity_type/source_field
// wiring beyond passing sourceDef through (already done for
// TargetFilter/MustMatchParentField) is needed for a picker to pick this
// up. Uses two DIFFERENT real StatusTypes specifically so a narrowing
// bug that returns everything is caught, not just one that narrows to
// the wrong-but-still-single StatusType.
func TestReferenceSearch_SourceField_StatusIDAutoScopedToOwnStatusType(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, tenantDB := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := foundation.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	if err := purchasing.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishForms: %v", err)
	}
	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishStatuses: %v", err)
	}
	if err := sales.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.Publish: %v", err)
	}
	if err := sales.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.PublishForms: %v", err)
	}
	if err := sales.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.PublishStatuses: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET",
		"/api/references/Status?source_entity_type=PurchaseOrder&source_field=status_id",
		tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	// purchase_order_status seeds exactly 5: Draft, Submitted, Approved,
	// Received, Cancelled (purchasing/seed.go PublishStatuses).
	if len(opts) != 5 {
		t.Fatalf("expected exactly the 5 purchase_order_status statuses, got %d: %+v", len(opts), opts)
	}
	wantLabels := map[string]bool{"Draft": true, "Submitted": true, "Approved": true, "Received": true, "Cancelled": true}
	for _, o := range opts {
		if !wantLabels[o.Label] {
			t.Fatalf("unexpected status leaked into PurchaseOrder's own status_id picker: %+v (full list %+v)", o, opts)
		}
	}
	// sales_order_status's own-named statuses (Confirmed, Fulfilled,
	// Invoiced — NOT shared with purchase_order_status, unlike
	// Draft/Cancelled) must never appear: the concrete proof this is
	// narrowed, not just coincidentally short.
	for _, o := range opts {
		if o.Label == "Confirmed" || o.Label == "Fulfilled" || o.Label == "Invoiced" {
			t.Fatalf("sales_order_status's own status leaked into PurchaseOrder's status_id picker: %+v", o)
		}
	}

	// The COMPLEMENTARY query — SalesOrder's own status_id picker — must
	// narrow to the OPPOSITE set (independent review, ADR-0032/
	// uc-infra#250: the single-sourceDef version of this test couldn't
	// distinguish "narrowed by sourceDef.StatusTypeCode" from "narrowed
	// to whichever StatusType happened to seed first"; querying a SECOND
	// real entity and asserting the sets swap is what actually proves
	// it's keyed on sourceDef).
	req2 := newRequest("GET",
		"/api/references/Status?source_entity_type=SalesOrder&source_field=status_id",
		tenantID, "farshid", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	opts2 := decodeRefOptions(t, rec2.Body.Bytes())
	// sales_order_status seeds exactly 5: Draft, Confirmed, Fulfilled,
	// Invoiced, Cancelled (sales/seed.go PublishStatuses).
	if len(opts2) != 5 {
		t.Fatalf("expected exactly the 5 sales_order_status statuses, got %d: %+v", len(opts2), opts2)
	}
	for _, o := range opts2 {
		if o.Label == "Submitted" || o.Label == "Approved" || o.Label == "Received" {
			t.Fatalf("purchase_order_status's own status leaked into SalesOrder's status_id picker: %+v", o)
		}
	}
}

// TestReferenceSearch_SourceField_StatusIDUnpublishedStatusTypeIs400Not500
// is the regression test for the second independent-review finding on
// this task: an unresolvable StatusTypeCode (purchasing published, but
// purchasing.PublishStatuses never run — modulebundle.Install's own doc
// comment documents this exact non-atomic, resumable window as a real,
// reachable state, not a hypothetical) used to fall through to
// writeInternalError → 500, even though every OTHER call site
// classifies the identical crud.ErrInvalidTransition as a 400
// (handlers.go's create/updateRecord). source_entity_type/source_field
// are caller-supplied query params, so this was reachable by any
// authenticated user, not just an edge case an operator might hit.
func TestReferenceSearch_SourceField_StatusIDUnpublishedStatusTypeIs400Not500(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, tenantDB := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := foundation.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	if err := purchasing.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishForms: %v", err)
	}
	// Deliberately NOT calling purchasing.PublishStatuses — PurchaseOrder
	// is published with StatusTypeCode "purchase_order_status" set, but
	// no StatusType row exists yet for this tenant.

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET",
		"/api/references/Status?source_entity_type=PurchaseOrder&source_field=status_id",
		tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (a tenant-provisioning state, not a server fault), got %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal error envelope: %v", err)
	}
	if env.Error == nil || *env.Error == "" {
		t.Fatalf("expected a non-empty error message, got %+v", env)
	}
}
