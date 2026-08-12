package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/authz"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/csvimport"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/sqlsource"
)

// TestAPI_FieldPermission_ImportCommitWithHiddenRequiredFieldDoesNotNameHiddenField
// is uc-infra#200's own regression test: the CSV/XLSX bulk-import commit
// path (import.go's importCommit) validates every row against the FULL,
// unredacted Definition — deliberately, same reasoning as
// createRecord/updateRecord (uc-infra#178) — because the mapping UI never
// offers a hidden field as an import target, so a hidden Required field
// is always submitted-absent, never restored from a legacy stored value
// the way a record update can restore one (the "milder" of uc-infra#200's
// two call sites, per that issue's own body). Before this fix,
// buildResultRows rendered res.Err.Error() raw — the untranslated,
// field-naming entity.ValidationError.Detail — for every row, leaking
// "note" to an actor who cannot see it anywhere else in the app.
func TestAPI_FieldPermission_ImportCommitWithHiddenRequiredFieldDoesNotNameHiddenField(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, legacyHiddenRequiredEntityDef(), legacyHiddenRequiredFormDef())
	seedFieldRule(t, db, "clerk4", "user-clerk4", "LegacyHiddenRequired", "note")

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// "note" is Required but hidden from user-clerk4 — the mapping never
	// offers it, so it goes into this commit completely unmapped.
	// ValidateMapping (csvimport.go) catches that request-level, BEFORE
	// any row is processed (its own doc comment: one clear error instead
	// of N identical per-row failures) — this is the second, subtler leak
	// this fix closes, found by this very test failing against the
	// per-row buildResultRows fix alone: a *csvimport.MappingError naming
	// "note" reached httpx.WriteError raw, the same disclosure shape one
	// layer earlier than the row-result rendering the issue's own body
	// named.
	csvContent := []byte("name\nDana\n")
	req := newMultipartRequest(t, "/import/LegacyHiddenRequired/commit", tenantID, "user-clerk4",
		"rows.csv", csvContent, map[string]string{"mapping.name": "name"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a Required field with no mapped column, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "note") {
		t.Fatalf("import result leaked the hidden field's name to an actor who cannot see it: %s", body)
	}
	if want := "administrator"; !strings.Contains(body, want) {
		t.Fatalf("expected the generic hidden_field_blocked message (containing %q), got: %s", want, body)
	}

	// An actor who CAN see "note" gets the real, actionable field name —
	// same request, same missing mapping, only disclosure differs.
	req2 := newMultipartRequest(t, "/import/LegacyHiddenRequired/commit", tenantID, "user-open",
		"rows.csv", csvContent, map[string]string{"mapping.name": "name"})
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if body2 := rec2.Body.String(); !strings.Contains(body2, "note") {
		t.Fatalf("unrestricted actor lost the actionable field name: %s", body2)
	}
}

// TestAPI_FieldPermission_ImportCommitMappingToHiddenFieldIsRefused is the
// regression test for the field-existence ORACLE independent review found
// in this fix's first version: importCommit deliberately keeps the FULL,
// unredacted Definition (see its own doc comment), so
// csvimport.ValidateMapping's def.FieldByName lookup accepted a hidden
// field as an explicit mapping target too — distinguishable, one row at a
// time, from a mapping target that doesn't exist at all
// ("...doesn't exist on entity..." vs. a 200 with an authz.ErrDenied row
// error naming the real field), letting an actor confirm a guessed hidden
// field's existence without ever seeing it anywhere else in the app. The
// fix refuses any mapping that targets a hidden field outright, before a
// single row is processed.
func TestAPI_FieldPermission_ImportCommitMappingToHiddenFieldIsRefused(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, legacyHiddenRequiredEntityDef(), legacyHiddenRequiredFormDef())
	seedFieldRule(t, db, "clerk5", "user-clerk5", "LegacyHiddenRequired", "note")

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// user-clerk5 explicitly maps "secret" (the CSV column) onto "note"
	// (the hidden field) — never something the real mapping UI would ever
	// submit, but nothing stops a hand-built request from trying.
	csvContent := []byte("name,secret\nDana,anything\n")
	req := newMultipartRequest(t, "/import/LegacyHiddenRequired/commit", tenantID, "user-clerk5",
		"rows.csv", csvContent, map[string]string{"mapping.name": "name", "mapping.secret": "note"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 refusing a mapping that targets a hidden field, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "note") {
		t.Fatalf("refusal leaked the hidden field's name: %s", body)
	}
	if want := "administrator"; !strings.Contains(body, want) {
		t.Fatalf("expected the generic hidden_field_blocked message (containing %q), got: %s", want, body)
	}

	// user-open CAN see "note" — the identical mapping actually commits.
	req2 := newMultipartRequest(t, "/import/LegacyHiddenRequired/commit", tenantID, "user-open",
		"rows.csv", csvContent, map[string]string{"mapping.name": "name", "mapping.secret": "note"})
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 for an unrestricted actor's ordinary mapping, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if body2 := rec2.Body.String(); !strings.Contains(body2, "1") {
		t.Fatalf("expected 1 row to succeed, got: %s", body2)
	}
}

// TestExtSQLImport_KeyedCommit_NotBeforeHiddenOtherFieldDoesNotNameHiddenField
// is buildUpsertResultRows' own end-to-end regression test (uc-infra#200,
// independent review finding 4) — the one shape independent review proved
// IS reachable via a real keyed SQL-source re-import: a visible field's
// own NotBefore rule still names a HIDDEN OtherField, because that rule
// lives on the visible field's own Field entry, unaffected by the hidden
// field's own entry being stripped out of def.Fields entirely. Driven
// against real Postgres with no fake driver, same technique
// TestExtSQLImport_Commit_WithKeyColumnIsIdempotent uses: register the
// test suite's own Postgres as an external source, seed a legacy row
// whose hidden "customs_date" already fails "received_date" 's NotBefore
// rule, then re-import it by key as the actor that date is hidden from.
func TestExtSQLImport_KeyedCommit_NotBeforeHiddenOtherFieldDoesNotNameHiddenField(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, notBeforeHiddenOtherFieldEntityDef(), notBeforeHiddenOtherFieldFormDef())
	seedFieldRule(t, db, "clerk6", "user-clerk6", "NotBeforeHiddenOther", "customs_date")

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	// A legacy row already in violation: received_date (2020-01-01) is
	// before customs_date (2020-06-01) — written through the raw repo,
	// same reasoning legacyHiddenRequiredFixture uses (crud.Engine.Create
	// would itself refuse to create a row already shaped like this).
	rec, err := data.NewRecordRepo(db).Create(ctx, "NotBeforeHiddenOther", map[string]any{
		"name": "Dana", "customs_date": "2020-06-01", "received_date": "2020-01-01",
	})
	if err != nil {
		t.Fatalf("seed legacy NotBefore-violating row: %v", err)
	}

	createScratchTable(t, db,
		`CREATE TABLE ext_notbefore ("ext_id" text, "name" text)`,
		fmt.Sprintf(`INSERT INTO ext_notbefore VALUES ('%s', 'Dana Renamed')`, rec.ID))
	// Seed the ExternalIdentity row directly so this run's re-import is a
	// genuine UPDATE (the branch that calls GuardedEngine.Update and
	// merges in the restored, real customs_date) rather than a first-run
	// Create — see upsert.go's own "known key -> Update" branch. A
	// validated Create (not the raw repo) is fine here: ExternalIdentity
	// itself has no NotBefore rule to trip.
	if _, err := crud.NewEngine(db).Create(ctx, foundation.ExternalIdentity(), map[string]any{
		"source_id": sourceID, "source_relation": "public.ext_notbefore",
		"entity_type": "NotBeforeHiddenOther", "record_id": rec.ID, "external_key": rec.ID,
	}, humanActor()); err != nil {
		t.Fatalf("seed ExternalIdentity: %v", err)
	}

	// Keyed commit as user-clerk6 (customs_date hidden from this actor):
	// only "name" is mapped (customs_date/received_date aren't source
	// columns at all), so the merge in upsert.go's Update branch is
	// current.Data (redacted: no customs_date) ∪ {"name": ...} —
	// EffectiveWriteFields is what puts customs_date's real stored value
	// back before re-validating.
	vals := url.Values{
		"source_id": {sourceID}, "schema": {"public"}, "relation": {"ext_notbefore"},
		"mapping.name": {"name"}, "key_column": {"ext_id"}, "key_column_set": {"1"},
	}
	req := newRequest("POST", "/import/NotBeforeHiddenOther/sql/commit", tenantID, "user-clerk6", []byte(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	restrictedRec := httptest.NewRecorder()
	mux.ServeHTTP(restrictedRec, req)
	if restrictedRec.Code != http.StatusOK {
		t.Fatalf("expected 200 (per-row failures, not a request-level error), got %d: %s", restrictedRec.Code, restrictedRec.Body.String())
	}
	body := restrictedRec.Body.String()
	if strings.Contains(body, "customs_date") {
		t.Fatalf("keyed SQL import result leaked the hidden OtherField's name: %s", body)
	}
	if want := "administrator"; !strings.Contains(body, want) {
		t.Fatalf("expected the generic hidden_field_blocked message (containing %q), got: %s", want, body)
	}
}

// TestBuildResultRows_HiddenFieldRedactionAndTranslation is a direct,
// pure-function test of h.buildResultRows (uc-infra#200) — the same
// isolation TestValidationErrorMessage_HiddenFieldGetsGenericMessage uses
// for validationErrorMessage itself, one level up the call chain: no
// HTTP, no DB, no csvimport machinery, just the row-rendering function
// every import surface (CSV preview/commit, SQL-source preview) shares.
func TestBuildResultRows_HiddenFieldRedactionAndTranslation(t *testing.T) {
	router := newTestRouter(t)
	h := testHandler(t, router)

	verr := &entity.ValidationError{
		Kind:       entity.KindRequired,
		EntityType: "LegacyHiddenRequired",
		FieldName:  "note",
		Detail:     `field "note" is required`,
	}
	results := []csvimport.RowResult{
		{RowNumber: 1, Data: map[string]any{"name": "Dana"}, Err: verr},
		{RowNumber: 2, Data: map[string]any{"name": "Sam"}},
	}

	// hidden: the row's error becomes the generic message, no field name.
	rows := h.buildResultRows("en", results, "OK", "Error", map[string]bool{"note": true})
	if strings.Contains(rows[0].Error, "note") {
		t.Fatalf("hidden field name leaked into the row message: %q", rows[0].Error)
	}
	if want := "administrator"; !strings.Contains(rows[0].Error, want) {
		t.Fatalf("expected the generic hidden_field_blocked message (containing %q), got: %q", want, rows[0].Error)
	}
	if !rows[1].OK || rows[1].Status != "OK" {
		t.Fatalf("expected row 2 (no error) to be reported OK, got: %+v", rows[1])
	}

	// nil hidden: byte-identical to the pre-fix behavior for a caller that
	// has nothing to redact (importPreview's def-already-redacted call
	// site, or any future caller) — this is the "existing behavior is
	// unchanged" regression guard uc-infra#178's own doc comment on
	// validationErrorMessage promises, one layer up.
	rows = h.buildResultRows("en", results, "OK", "Error", nil)
	if want := "note is required."; rows[0].Error != want {
		t.Fatalf("buildResultRows(nil hidden) = %q, want %q", rows[0].Error, want)
	}

	// A non-ValidationError falls through to err.Error() unchanged either
	// way — validationErrorMessage's own documented fallback.
	plain := errors.New("boom: connection reset")
	rows = h.buildResultRows("en", []csvimport.RowResult{{RowNumber: 1, Err: plain}}, "OK", "Error", map[string]bool{"note": true})
	if rows[0].Error != plain.Error() {
		t.Fatalf("non-ValidationError row error should pass through unchanged, got %q, want %q", rows[0].Error, plain.Error())
	}
}

// TestBuildUpsertResultRows_HiddenFieldRedactionAndSORUnaffected is
// buildResultRows' own test above, for the SQL-source keyed-upsert
// sibling (uc-infra#200's other, harder-to-reach call site — see this
// package's code review record for why the plain FieldName-hidden shape
// turns out NOT to be reachable there in practice, def already being
// redacted ahead of validation, but the KindNotBefore/OtherField shape
// still is). Also confirms the pre-existing
// authz.ErrSystemOfRecordReadOnly branch (sorReadOnlyMessage) is
// untouched by threading hidden through — that branch never reaches
// validationErrorMessage at all.
func TestBuildUpsertResultRows_HiddenFieldRedactionAndSORUnaffected(t *testing.T) {
	router := newTestRouter(t)
	h := testHandler(t, router)

	notBefore := &entity.ValidationError{
		Kind:       entity.KindNotBefore,
		EntityType: "NotBeforeHiddenOther",
		FieldName:  "received_date",
		OtherField: "customs_date",
		Detail:     `field "received_date" must not be before "customs_date"`,
	}
	results := []sqlsource.UpsertResult{
		{RowResult: csvimport.RowResult{RowNumber: 1, Err: notBefore}, Updated: true},
		{RowResult: csvimport.RowResult{RowNumber: 2, Err: authz.ErrSystemOfRecordReadOnly}},
	}

	rows := h.buildUpsertResultRows("en", results, "Created", "Updated", "Error", map[string]bool{"customs_date": true})
	if strings.Contains(rows[0].Error, "customs_date") {
		t.Fatalf("hidden OtherField leaked into the row message: %q", rows[0].Error)
	}
	if want := "administrator"; !strings.Contains(rows[0].Error, want) {
		t.Fatalf("expected the generic hidden_field_blocked message (containing %q), got: %q", want, rows[0].Error)
	}
	// The system-of-record branch takes priority over validationErrorMessage
	// entirely (errors.Is check runs first) — hidden must not change that.
	if rows[1].Error == "" || strings.Contains(rows[1].Error, "administrator") {
		t.Fatalf("expected the SOR read-only message, unaffected by hidden, got: %q", rows[1].Error)
	}

	// nil hidden: the OtherField is named as it always was pre-fix.
	rows = h.buildUpsertResultRows("en", results, "Created", "Updated", "Error", nil)
	if !strings.Contains(rows[0].Error, "customs_date") {
		t.Fatalf("buildUpsertResultRows(nil hidden) should still name the field, got: %q", rows[0].Error)
	}
}
