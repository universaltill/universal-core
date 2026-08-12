package csvimport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

func vendorDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Vendor",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "lead_time_days", Type: entity.FieldNumber},
			{Name: "is_active", Type: entity.FieldBool, Default: true},
			{Name: "rating", Type: entity.FieldEnum, EnumValues: []string{"gold", "silver", "bronze"}},
		},
	}
}

func vendorMapping() ColumnMapping {
	return ColumnMapping{
		"Vendor Name": "name",
		"Lead Time":   "lead_time_days",
		"Active":      "is_active",
		"Rating":      "rating",
	}
}

func TestPreview_ValidRowsPassValidation(t *testing.T) {
	csvData := "Vendor Name,Lead Time,Active,Rating\n" +
		"Acme Textiles,60,true,gold\n" +
		"Beta Supplies,45,false,silver\n"

	results, err := Preview(strings.NewReader(csvData), vendorDef(), vendorMapping())
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("row %d: expected no error, got %v", r.RowNumber, r.Err)
		}
	}
	if results[0].Data["name"] != "Acme Textiles" {
		t.Fatalf("unexpected row 1 data: %+v", results[0].Data)
	}
	if results[0].Data["lead_time_days"] != 60.0 {
		t.Fatalf("expected lead_time_days coerced to float64(60), got %v (%T)", results[0].Data["lead_time_days"], results[0].Data["lead_time_days"])
	}
	if results[1].Data["is_active"] != false {
		t.Fatalf("expected is_active coerced to bool(false), got %v (%T)", results[1].Data["is_active"], results[1].Data["is_active"])
	}
}

func TestPreview_RowNumbersAreOneBasedAndExcludeHeader(t *testing.T) {
	csvData := "Vendor Name\nFirst\nSecond\nThird\n"
	results, err := Preview(strings.NewReader(csvData), vendorDef(), ColumnMapping{"Vendor Name": "name"})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	for i, want := range []int{1, 2, 3} {
		if results[i].RowNumber != want {
			t.Fatalf("row index %d: expected RowNumber %d, got %d", i, want, results[i].RowNumber)
		}
	}
}

func TestPreview_BadRowDoesNotBlockOtherRows(t *testing.T) {
	csvData := "Vendor Name,Lead Time\n" +
		"Acme,not-a-number\n" +
		"Beta,30\n"

	results, err := Preview(strings.NewReader(csvData), vendorDef(), ColumnMapping{"Vendor Name": "name", "Lead Time": "lead_time_days"})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 rows reported, got %d", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("expected row 1 (bad lead_time_days) to have an error")
	}
	if results[1].Err != nil {
		t.Fatalf("expected row 2 to still validate despite row 1 failing, got %v", results[1].Err)
	}
}

func TestPreview_MissingRequiredFieldFailsThatRow(t *testing.T) {
	csvData := "Vendor Name,Lead Time\n,30\n"
	results, err := Preview(strings.NewReader(csvData), vendorDef(), ColumnMapping{"Vendor Name": "name", "Lead Time": "lead_time_days"})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if results[0].Err == nil {
		t.Fatal("expected error for empty required name field")
	}
}

// TestPreview_RequiredI18nTextFieldWithNoContentFailsThatRow is the
// csvimport-layer half of uc-infra#104. A genuinely BLANK cell is already
// caught upstream of this bug: buildRowData treats an empty cell as an
// absent field (skips Coerce entirely), which the pre-existing presence
// check in ValidateRecord already rejects for a Required field. The gap
// is a cell whose text IS a validly-shaped but content-less JSON object —
// "{}", or every locale blank — which Coerce parses into a PRESENT {}
// (or all-blank) value that the old code let straight through.
func TestPreview_RequiredI18nTextFieldWithNoContentFailsThatRow(t *testing.T) {
	def := &entity.Definition{
		EntityType: "Unit",
		Version:    1,
		Fields: []entity.Field{
			{Name: "label", Type: entity.FieldI18nText, Required: true},
			{Name: "code", Type: entity.FieldString},
		},
	}
	mapping := ColumnMapping{"Label": "label", "Code": "code"}

	// A genuinely blank cell was already caught before this fix — kept
	// here as a regression guard against that pre-existing behavior
	// changing, not as this bug's own reproduction.
	csvData := "Label,Code\n,ea\n"
	results, err := Preview(strings.NewReader(csvData), def, mapping)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "required") {
		t.Fatalf("expected a 'required' error for a blank cell on a required i18n_text field, got %v", results[0].Err)
	}

	// The actual gap: a cell containing "{}" — valid JSON, coerces to a
	// PRESENT empty object, not an absent field.
	csvData = "Label,Code\n" + `"{}",ea` + "\n"
	results, err = Preview(strings.NewReader(csvData), def, mapping)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "required") {
		t.Fatalf(`expected a 'required' error for a required i18n_text cell of "{}" (present but content-less), got %v`, results[0].Err)
	}

	// Same gap, one locale present but blank. Note the CSV-quoting: to
	// embed the JSON text {"en":""} inside a quoted CSV field, every "
	// doubles ({""en"":""""}) — a single-doubled `""` around an empty
	// value would decode to the JSON text {"en":"} (an unterminated
	// string), which fails as malformed JSON rather than exercising the
	// present-but-blank check this test means to cover (independent
	// review, uc-infra#104: an earlier draft of this test got this
	// wrong and passed for the wrong reason).
	csvData = "Label,Code\n" + `"{""en"":""""}",ea` + "\n"
	results, err = Preview(strings.NewReader(csvData), def, mapping)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "required") {
		t.Fatalf(`expected a 'required' error for a required i18n_text cell with its one locale blank, got %v`, results[0].Err)
	}

	// Same gap, every locale present but blank (multi-locale).
	csvData = "Label,Code\n" + `"{""en"":"""",""tr"":""""}",ea` + "\n"
	results, err = Preview(strings.NewReader(csvData), def, mapping)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "required") {
		t.Fatalf(`expected a 'required' error for a required i18n_text cell with every locale blank, got %v`, results[0].Err)
	}

	// A cell with real content in at least one locale still imports fine.
	csvData = "Label,Code\n" + `"{""en"":""Each""}",ea` + "\n"
	results, err = Preview(strings.NewReader(csvData), def, mapping)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("expected a non-blank i18n_text cell to pass, got %v", results[0].Err)
	}
}

func TestPreview_UnknownEnumValueFails(t *testing.T) {
	csvData := "Vendor Name,Rating\nAcme,platinum\n"
	results, err := Preview(strings.NewReader(csvData), vendorDef(), ColumnMapping{"Vendor Name": "name", "Rating": "rating"})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if results[0].Err == nil {
		t.Fatal("expected error for rating value not in the declared enum")
	}
}

func TestPreview_ShortRowLeavesTrailingFieldsAbsent(t *testing.T) {
	// "Active" and "Rating" columns declared in the header but this row
	// doesn't have values for them — should not panic or index out of range.
	csvData := "Vendor Name,Lead Time,Active,Rating\nAcme,30\n"
	results, err := Preview(strings.NewReader(csvData), vendorDef(), vendorMapping())
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("expected short row to validate fine (no required fields missing), got %v", results[0].Err)
	}
	if _, present := results[0].Data["is_active"]; present {
		t.Fatalf("expected is_active to be absent for a short row, got %v", results[0].Data["is_active"])
	}
}

func TestValidateMapping_RejectsUnknownCSVColumn(t *testing.T) {
	err := ValidateMapping(vendorDef(), []string{"Vendor Name"}, ColumnMapping{"Typo Column": "name"})
	if err == nil {
		t.Fatal("expected error for a mapping source column not present in the CSV header")
	}
}

func TestValidateMapping_RejectsUnknownEntityField(t *testing.T) {
	err := ValidateMapping(vendorDef(), []string{"Vendor Name"}, ColumnMapping{"Vendor Name": "not_a_real_field"})
	if err == nil {
		t.Fatal("expected error for a mapping target field that doesn't exist on the entity")
	}
}

func TestValidateMapping_RejectsUnmappedRequiredField(t *testing.T) {
	// "name" is required on vendorDef but nothing maps to it.
	err := ValidateMapping(vendorDef(), []string{"Lead Time"}, ColumnMapping{"Lead Time": "lead_time_days"})
	if err == nil {
		t.Fatal("expected error: required field name has no mapped column")
	}
	// Structured, not just a formatted string (uc-infra#200): a caller
	// with a hidden-field set to redact against (import.go's importCommit)
	// needs FieldName via errors.As, not string-parsing, to tell whether
	// this specific failure is safe to show an actor. Error()'s wording
	// stays pinned too — extsqlimport.go's/import.go's mapping-error UI
	// fragment renders it verbatim, so a silent wording change would be a
	// user-facing regression this test would otherwise miss.
	var mapErr *MappingError
	if !errors.As(err, &mapErr) {
		t.Fatalf("expected a *MappingError, got %T: %v", err, err)
	}
	if mapErr.FieldName != "name" {
		t.Fatalf("MappingError.FieldName = %q, want %q", mapErr.FieldName, "name")
	}
	if want := `required field "name" has no column mapped to it`; err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestValidateMapping_ErrorPreventsPerRowNoise(t *testing.T) {
	// A broken mapping should surface as ONE top-level error from Preview,
	// not one per row.
	csvData := "Vendor Name\nAcme\nBeta\nGamma\n"
	_, err := Preview(strings.NewReader(csvData), vendorDef(), ColumnMapping{"Vendor Name": "not_a_real_field"})
	if err == nil {
		t.Fatal("expected Preview to fail fast on a broken mapping rather than validating rows against it")
	}
}

func TestPreview_EmptyCSVHasNoHeaderRow(t *testing.T) {
	_, err := Preview(strings.NewReader(""), vendorDef(), vendorMapping())
	if err == nil {
		t.Fatal("expected error for a CSV with no header row")
	}
}

func TestPreview_RaggedRowReportedNotFatal(t *testing.T) {
	// FieldsPerRecord = -1 tolerates a short/ragged row at the csv.Reader
	// level; make sure the package doesn't then panic building row data.
	csvData := "Vendor Name,Lead Time,Active,Rating\nAcme\nBeta,30,true,gold\n"
	results, err := Preview(strings.NewReader(csvData), vendorDef(), vendorMapping())
	if err != nil {
		t.Fatalf("Preview should not fail the whole batch on one ragged row: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(results))
	}
	if results[1].Err != nil {
		t.Fatalf("expected the well-formed second row to still validate: %v", results[1].Err)
	}
}

// TestValidateMapping_RejectsDuplicateTargetField is the regression test
// for the code-review finding that two CSV columns mapped to the same
// entity field would silently clobber each other nondeterministically
// (Go's map iteration order is randomized, so whichever column happened
// to be visited last in buildRowData's range loop would win, differing
// unpredictably row to row within the same import).
func TestValidateMapping_RejectsDuplicateTargetField(t *testing.T) {
	err := ValidateMapping(vendorDef(), []string{"Col A", "Col B"}, ColumnMapping{"Col A": "name", "Col B": "name"})
	if err == nil {
		t.Fatal("expected error: two columns mapped to the same target field")
	}
}

func TestPreview_EmptyMappingWritesNothingButDoesNotError(t *testing.T) {
	def := &entity.Definition{
		EntityType: "NoRequiredFields",
		Fields:     []entity.Field{{Name: "note", Type: entity.FieldString}},
	}
	csvData := "Note\nanything\nanything else\n"
	results, err := Preview(strings.NewReader(csvData), def, ColumnMapping{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 rows reported, got %d", len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("expected an empty mapping (no Required fields) to validate every row, got %v", r.Err)
		}
		if len(r.Data) != 0 {
			t.Fatalf("expected no fields populated with an empty mapping, got %+v", r.Data)
		}
	}
}

// TestPreview_StripsUTF8BOMFromFirstHeader is the regression test for the
// code-review finding that a "CSV UTF-8" export from Excel (which
// prefixes the file with a byte-order mark) would otherwise silently
// break every mapping targeting the first column, since the BOM bytes
// would be invisibly glued onto that header's name.
func TestPreview_StripsUTF8BOMFromFirstHeader(t *testing.T) {
	csvData := "\uFEFFVendor Name,Lead Time\nAcme,60\n"
	results, err := Preview(strings.NewReader(csvData), vendorDef(), ColumnMapping{"Vendor Name": "name", "Lead Time": "lead_time_days"})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("expected the BOM-prefixed header to still match the mapping, got %v", results[0].Err)
	}
	if results[0].Data["name"] != "Acme" {
		t.Fatalf("unexpected data: %+v", results[0].Data)
	}
}

// freshTenantDB opens a connection to a brand-new tenant database
// (ADR-0003), skipping (not failing) if TEST_DATABASE_URL isn't set —
// same convention as crud_test.go.
func freshTenantDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	name := fmt.Sprintf("uc_test_csvimport_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	tenantDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open tenant database %s: %v", name, err)
	}
	t.Cleanup(func() { tenantDB.Close() })
	if err := tenantDB.Ping(); err != nil {
		t.Fatalf("ping tenant database %s: %v", name, err)
	}
	if _, err := tenantDB.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyTenant(context.Background(), tenantDB); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}
	return tenantDB
}

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
}

func TestCommit_WritesOnlyRowsThatPassValidation(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := crud.NewEngine(db)
	def := vendorDef()

	csvData := "Vendor Name,Lead Time\n" +
		"Acme,60\n" +
		",30\n" + // missing required name — should be skipped
		"Beta,not-a-number\n" + // bad number — should be skipped
		"Gamma,45\n"

	results, err := Commit(ctx, strings.NewReader(csvData), def, ColumnMapping{"Vendor Name": "name", "Lead Time": "lead_time_days"}, engine, humanActor())
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("expected 4 row results, got %d", len(results))
	}

	if results[0].Err != nil || results[0].RecordID == "" {
		t.Fatalf("expected row 1 (Acme) to commit successfully, got err=%v recordID=%q", results[0].Err, results[0].RecordID)
	}
	if results[1].Err == nil || results[1].RecordID != "" {
		t.Fatalf("expected row 2 (missing name) to be skipped, got err=%v recordID=%q", results[1].Err, results[1].RecordID)
	}
	if results[2].Err == nil || results[2].RecordID != "" {
		t.Fatalf("expected row 3 (bad number) to be skipped, got err=%v recordID=%q", results[2].Err, results[2].RecordID)
	}
	if results[3].Err != nil || results[3].RecordID == "" {
		t.Fatalf("expected row 4 (Gamma) to still commit despite rows 2-3 failing, got err=%v recordID=%q", results[3].Err, results[3].RecordID)
	}

	// Exactly the 2 good rows actually landed in the database.
	got, err := engine.List(ctx, def)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 records written (bad rows must not land), got %d", len(got))
	}
}

// TestCommit_WritesAuditRowsPerRecord confirms each committed row goes
// through crud.Engine.Create — meaning it gets an audit_log row with
// actor identity — not a bulk bypass around the normal write path.
func TestCommit_WritesAuditRowsPerRecord(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := crud.NewEngine(db)
	def := vendorDef()
	csvData := "Vendor Name\nAcme\nBeta\n"
	// uc-infra#161: this package has no production ActorAgent caller
	// today (internal/api/import.go always passes the request's human
	// rc.Actor) — this fixture's Input is a representative placeholder,
	// not a prescribed convention. In particular it must NOT become "hash
	// the raw file content": Commit calls engine.Create per row
	// (csvimport.go), and each Create recomputes actor.InputHash() —
	// hashing a whole file's bytes on every one of its own rows would be
	// O(rows × file size), not O(file size). A real ActorAgent import
	// caller should use a small batch descriptor (e.g. filename + row
	// count) as Input, the same shape as CLIInvocationInput's own
	// resolved-invocation string, not the payload itself.
	actor := audit.Actor{Type: audit.ActorAgent, ID: "csv-import-agent", ModelVersion: "claude-fable-5", Input: "import vendors.csv (2 rows)"}

	results, err := Commit(ctx, strings.NewReader(csvData), def, ColumnMapping{"Vendor Name": "name"}, engine, actor)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var auditCount int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE entity_type = 'Vendor' AND actor_type = 'ai_agent'`,
	).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if auditCount != len(results) {
		t.Fatalf("expected %d audit rows (one per committed row), got %d", len(results), auditCount)
	}
}

// TestPreviewRows_MatchesFileBasedPreview pins the contract ADR-0019
// leans on: the rows-based entry points (what an external SQL relation
// feeds) and the file-based ones (what an upload feeds) must be the
// same engine, not two engines that can drift.
func TestPreviewRows_MatchesFileBasedPreview(t *testing.T) {
	def := vendorDef()
	mapping := ColumnMapping{"Vendor Name": "name", "Lead Time": "lead_time_days"}
	csvData := "Vendor Name,Lead Time\nAcme,60\n,30\nBeta,not-a-number\n"

	fromFile, err := Preview(strings.NewReader(csvData), def, mapping)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	headers := []string{"Vendor Name", "Lead Time"}
	rows := [][]string{{"Acme", "60"}, {"", "30"}, {"Beta", "not-a-number"}}
	fromRows, err := PreviewRows(headers, rows, def, mapping)
	if err != nil {
		t.Fatalf("PreviewRows: %v", err)
	}

	if len(fromFile) != len(fromRows) {
		t.Fatalf("result count differs: file=%d rows=%d", len(fromFile), len(fromRows))
	}
	for i := range fromFile {
		if (fromFile[i].Err == nil) != (fromRows[i].Err == nil) {
			t.Errorf("row %d: validity differs between file and rows path (file err=%v, rows err=%v)", i+1, fromFile[i].Err, fromRows[i].Err)
		}
		if fmt.Sprint(fromFile[i].Data) != fmt.Sprint(fromRows[i].Data) {
			t.Errorf("row %d: data differs between file and rows path (%v vs %v)", i+1, fromFile[i].Data, fromRows[i].Data)
		}
	}
}

func TestPreviewRows_RejectsBrokenMappingUpfront(t *testing.T) {
	def := vendorDef()
	_, err := PreviewRows([]string{"A"}, [][]string{{"x"}}, def, ColumnMapping{"Missing": "name"})
	if err == nil {
		t.Fatal("expected ValidateMapping to reject a mapping referencing an absent column")
	}
}

// TestCommitRows_WritesLikeFileBasedCommit — the rows path writes
// through the same engine.Create per row, skipping bad rows without
// blocking good ones, exactly like TestCommit_WritesOnlyRowsThatPassValidation.
func TestCommitRows_WritesLikeFileBasedCommit(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := crud.NewEngine(db)
	def := vendorDef()

	headers := []string{"Vendor Name", "Lead Time"}
	rows := [][]string{{"Acme", "60"}, {"", "30"}, {"Gamma", "45"}}
	results, err := CommitRows(ctx, headers, rows, def, ColumnMapping{"Vendor Name": "name", "Lead Time": "lead_time_days"}, engine, humanActor())
	if err != nil {
		t.Fatalf("CommitRows: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 row results, got %d", len(results))
	}
	if results[0].RecordID == "" || results[2].RecordID == "" {
		t.Fatalf("expected rows 1 and 3 to commit, got %+v", results)
	}
	if results[1].Err == nil || results[1].RecordID != "" {
		t.Fatalf("expected row 2 (missing required name) skipped, got %+v", results[1])
	}
	got, err := engine.List(ctx, def)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly the 2 valid rows written, got %d", len(got))
	}
}

// TestCommitRows_StopsOnContextCancellation — a deadline mid-batch must
// not grind through the remaining rows collecting identical driver
// errors; unattempted rows carry the context error honestly.
func TestCommitRows_StopsOnContextCancellation(t *testing.T) {
	db := freshTenantDB(t)
	engine := crud.NewEngine(db)
	def := vendorDef()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the first write

	headers := []string{"Vendor Name"}
	rows := [][]string{{"Acme"}, {"Beta"}}
	results, err := CommitRows(ctx, headers, rows, def, ColumnMapping{"Vendor Name": "name"}, engine, humanActor())
	if err != nil {
		t.Fatalf("CommitRows: %v", err)
	}
	for i, res := range results {
		if res.RecordID != "" {
			t.Errorf("row %d: committed despite cancelled context", i+1)
		}
		if res.Err == nil || !strings.Contains(res.Err.Error(), "context canceled") {
			t.Errorf("row %d: expected a context error, got %v", i+1, res.Err)
		}
	}
	got, err := engine.List(context.Background(), def)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no records written under a cancelled context, got %d", len(got))
	}
}
