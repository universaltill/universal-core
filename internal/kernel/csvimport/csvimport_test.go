package csvimport

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

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

// testDB opens the integration-test database, skipping (not failing) if
// TEST_DATABASE_URL isn't set — same convention as crud_test.go.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	return db
}

func seedTenant(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	err := db.QueryRow(
		`INSERT INTO tenants (name, region) VALUES ($1, $2) RETURNING id`,
		"Test Tenant", "eu-west",
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
}

func TestCommit_WritesOnlyRowsThatPassValidation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := crud.NewEngine(db)
	def := vendorDef()

	csvData := "Vendor Name,Lead Time\n" +
		"Acme,60\n" +
		",30\n" + // missing required name — should be skipped
		"Beta,not-a-number\n" + // bad number — should be skipped
		"Gamma,45\n"

	results, err := Commit(ctx, strings.NewReader(csvData), def, ColumnMapping{"Vendor Name": "name", "Lead Time": "lead_time_days"}, engine, tenantID, humanActor())
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
	got, err := engine.List(ctx, def, tenantID)
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
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := crud.NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorAgent, ID: "csv-import-agent", ModelVersion: "claude-fable-5"}

	csvData := "Vendor Name\nAcme\nBeta\n"
	results, err := Commit(ctx, strings.NewReader(csvData), def, ColumnMapping{"Vendor Name": "name"}, engine, tenantID, actor)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var auditCount int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND entity_type = 'Vendor' AND actor_type = 'ai_agent'`,
		tenantID,
	).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if auditCount != len(results) {
		t.Fatalf("expected %d audit rows (one per committed row), got %d", len(results), auditCount)
	}
}
