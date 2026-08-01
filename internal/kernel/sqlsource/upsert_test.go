package sqlsource

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/authz"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/csvimport"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

// RecordEngine must stay satisfied by both engines the import paths use:
// the RBAC-guarded engine for target records (a user-driven write path)
// and the raw crud engine for the ExternalIdentity side-channel — see
// RecordEngine's own doc comment. A signature drift on either breaks
// here, at compile time, not in a handler.
var (
	_ RecordEngine = (*crud.Engine)(nil)
	_ RecordEngine = (*authz.GuardedEngine)(nil)
)

// fakeEngine is an in-memory RecordEngine for unit-testing the upsert
// decision logic without Postgres (the real write path is covered by
// TestUpsert_ReImportUpdatesInsteadOfDuplicating in integration_test.go).
// The per-method fail* errors let one engine role fail one capability
// while the rest keep working — e.g. an identity Create failing while
// the pre-load ListByField on the same engine still succeeds.
type fakeEngine struct {
	nextID  int
	records []data.Record // all entity types together, like the generic records table
	updates int

	failCreate error
	failUpdate error
	failGet    error
	failList   error
}

func (f *fakeEngine) Create(_ context.Context, def *entity.Definition, fields map[string]any, _ audit.Actor) (data.Record, error) {
	if f.failCreate != nil {
		return data.Record{}, f.failCreate
	}
	f.nextID++
	rec := data.Record{
		ID:         fmt.Sprintf("rec-%d", f.nextID),
		EntityType: def.EntityType,
		Data:       fields,
		Version:    1,
	}
	f.records = append(f.records, rec)
	return rec, nil
}

func (f *fakeEngine) Update(_ context.Context, def *entity.Definition, id string, fields map[string]any, _ *int, _ audit.Actor) (int, error) {
	if f.failUpdate != nil {
		return 0, f.failUpdate
	}
	for i, rec := range f.records {
		if rec.EntityType == def.EntityType && rec.ID == id {
			// Full replacement, like data.RecordRepo.UpdateTx — merging is
			// the CALLER's job, which is exactly what the merge tests pin.
			f.records[i].Data = fields
			f.records[i].Version++
			f.updates++
			return f.records[i].Version, nil
		}
	}
	return 0, fmt.Errorf("record %s not found", id)
}

func (f *fakeEngine) Get(_ context.Context, def *entity.Definition, id string) (data.Record, error) {
	if f.failGet != nil {
		return data.Record{}, f.failGet
	}
	for _, rec := range f.records {
		if rec.EntityType == def.EntityType && rec.ID == id {
			return rec, nil
		}
	}
	return data.Record{}, fmt.Errorf("record %s not found", id)
}

func (f *fakeEngine) ListByField(_ context.Context, def *entity.Definition, fieldName, value string) ([]data.Record, error) {
	if f.failList != nil {
		return nil, f.failList
	}
	var out []data.Record
	for _, rec := range f.records {
		if rec.EntityType != def.EntityType {
			continue
		}
		if v, _ := rec.Data[fieldName].(string); v == value {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (f *fakeEngine) ofType(entityType string) []data.Record {
	var out []data.Record
	for _, rec := range f.records {
		if rec.EntityType == entityType {
			out = append(out, rec)
		}
	}
	return out
}

// seedIdentity plants an ExternalIdentity row directly (bypassing the
// engine under test), for update-path and corrupt-identity scenarios.
func (f *fakeEngine) seedIdentity(sourceID, relation, entityType, recordID, key string) {
	f.nextID++
	f.records = append(f.records, data.Record{
		ID:         fmt.Sprintf("rec-%d", f.nextID),
		EntityType: "ExternalIdentity",
		Data: map[string]any{
			"source_id": sourceID, "source_relation": relation,
			"entity_type": entityType, "record_id": recordID, "external_key": key,
		},
		Version: 1,
	})
}

var upsertTestActor = audit.Actor{Type: audit.ActorAgent, ID: "sqlsource-upsert-test", ModelVersion: "test"}

func itemImportShape() ([]string, csvimport.ColumnMapping) {
	// The post-ApplyConstants shape a NAV Item pull arrives in.
	headers := []string{"No_", "Description", "__const:item_type"}
	mapping := csvimport.ColumnMapping{"No_": "sku", "Description": "name", "__const:item_type": "item_type"}
	return headers, mapping
}

// partyImportShape is the NAV Customer/Vendor shape: the key column No_
// is NOT mapped to any target field (Party has no external-code field —
// the whole reason ExternalIdentity exists), which is what makes the
// missing-key and relation-scoping paths reachable at all.
func partyImportShape() ([]string, csvimport.ColumnMapping) {
	headers := []string{"No_", "Name", "__const:party_type"}
	mapping := csvimport.ColumnMapping{"Name": "name", "__const:party_type": "party_type"}
	return headers, mapping
}

// upsert runs CommitRowsUpserting with the same fake serving both engine
// roles — fine wherever a test doesn't care about the records/identities
// split (the error-path tests, which do, call the function directly).
func upsert(t *testing.T, eng *fakeEngine, def *entity.Definition, headers []string, mapping csvimport.ColumnMapping, rows [][]string, key, src, rel string) []UpsertResult {
	t.Helper()
	results, err := CommitRowsUpserting(context.Background(), headers, rows, def, mapping,
		key, src, rel, eng, eng, foundation.ExternalIdentity(), upsertTestActor)
	if err != nil {
		t.Fatal(err)
	}
	return results
}

func TestKeyIndex(t *testing.T) {
	idx, err := KeyIndex([]string{"No_", "Name"}, "Name")
	if err != nil || idx != 1 {
		t.Fatalf("expected index 1, got %d, %v", idx, err)
	}
	if _, err := KeyIndex([]string{"No_", "Name"}, "Item No_"); err == nil {
		t.Fatal("expected an error for a key column that is not a header")
	}
}

func TestMarkMissingKeys_AnnotatesBlankKeysAndPreservesErrors(t *testing.T) {
	headers, mapping := partyImportShape()
	rows := [][]string{
		{"C001", "Acme Ltd", "organization"},
		{"   ", "Keyless Ltd", "organization"}, // whitespace-only key: missing
		{"C003", "", "organization"},           // blank Name: preview's own error
	}
	previews, err := csvimport.PreviewRows(headers, rows, foundation.Party(), mapping)
	if err != nil {
		t.Fatal(err)
	}
	marked, err := MarkMissingKeys(headers, rows, "No_", previews)
	if err != nil {
		t.Fatal(err)
	}
	if marked[0].Err != nil {
		t.Fatalf("row 1 has a key and valid data, expected no error, got %v", marked[0].Err)
	}
	if marked[1].Err == nil || !strings.Contains(marked[1].Err.Error(), "missing key") {
		t.Fatalf("expected row 2 annotated with a missing-key error, got %v", marked[1].Err)
	}
	// A row already failing preview keeps ITS error, not a key overlay.
	if marked[2].Err == nil || strings.Contains(marked[2].Err.Error(), "missing key") {
		t.Fatalf("expected row 3 to keep its preview validation error, got %v", marked[2].Err)
	}
	// The input results are not mutated (preview reuse contract).
	if previews[1].Err != nil {
		t.Fatalf("MarkMissingKeys mutated its input results: %v", previews[1].Err)
	}
}

func TestMarkMissingKeys_RejectsBadKeyColumn(t *testing.T) {
	headers, mapping := partyImportShape()
	rows := [][]string{{"C001", "Acme Ltd", "organization"}}
	previews, err := csvimport.PreviewRows(headers, rows, foundation.Party(), mapping)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MarkMissingKeys(headers, rows, "Nope", previews); err == nil {
		t.Fatal("expected an error for a key column that is not a source header")
	}
}

func TestCommitRowsUpserting_RejectsKeyColumnNotInHeaders(t *testing.T) {
	headers, mapping := itemImportShape()
	eng := &fakeEngine{}
	_, err := CommitRowsUpserting(context.Background(), headers, [][]string{{"1000", "Bicycle", "stock"}},
		purchasing.Item(), mapping, "Item No_", "src-1", "dbo.A$Item", eng, eng,
		foundation.ExternalIdentity(), upsertTestActor)
	if err == nil {
		t.Fatal("expected an error for a key column that is not a source header")
	}
	if len(eng.records) != 0 {
		t.Fatalf("nothing should be written on a key-column error, got %d records", len(eng.records))
	}
}

func TestCommitRowsUpserting_EmptyKeyIsRowErrorOthersProceed(t *testing.T) {
	headers, mapping := partyImportShape()
	rows := [][]string{
		{"C001", "Acme Ltd", "organization"},
		{"   ", "Keyless Ltd", "organization"}, // whitespace-only key: missing
		{"C003", "Globex Ltd", "organization"},
	}
	eng := &fakeEngine{}
	results := upsert(t, eng, foundation.Party(), headers, mapping, rows, "No_", "src-1", "dbo.A$Customer")
	if results[1].Err == nil || !strings.Contains(results[1].Err.Error(), "missing key") {
		t.Fatalf("expected a missing-key row error, got %+v", results[1])
	}
	if results[1].RecordID != "" || results[1].Updated {
		t.Fatalf("the keyless row must not be written, got %+v", results[1])
	}
	for _, i := range []int{0, 2} {
		if results[i].Err != nil || results[i].RecordID == "" || results[i].Updated {
			t.Fatalf("row %d: expected a clean create, got %+v", i+1, results[i])
		}
	}
	if got := len(eng.ofType("Party")); got != 2 {
		t.Fatalf("expected 2 Party records, got %d", got)
	}
	if got := len(eng.ofType("ExternalIdentity")); got != 2 {
		t.Fatalf("expected 2 identity rows (one per created record), got %d", got)
	}
}

func TestCommitRowsUpserting_AmbiguousIdentityIsRowError(t *testing.T) {
	headers, mapping := itemImportShape()
	eng := &fakeEngine{}
	// Two identity rows for the same (source, relation, entity type, key)
	// — the app-level-uniqueness limitation ExternalIdentity's doc
	// comment records, materialized. Detected at pre-load; the engine
	// must refuse to guess.
	eng.seedIdentity("src-1", "dbo.A$Item", "Item", "rec-a", "1000")
	eng.seedIdentity("src-1", "dbo.A$Item", "Item", "rec-b", "1000")

	results := upsert(t, eng, purchasing.Item(), headers, mapping,
		[][]string{{"1000", "Bicycle", "stock"}}, "No_", "src-1", "dbo.A$Item")
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "ambiguous identity") {
		t.Fatalf("expected an ambiguous-identity row error, got %+v", results[0])
	}
	if results[0].RecordID != "" || results[0].Updated {
		t.Fatalf("an ambiguous row must not be written, got %+v", results[0])
	}
	if got := len(eng.ofType("Item")); got != 0 {
		t.Fatalf("an ambiguous row must not create a record either, got %d", got)
	}
}

// TestCommitRowsUpserting_SecondRunUpdatesFirstRunsRecords is the core
// idempotency contract at the unit level (the real-Postgres proof lives
// in integration_test.go): same pull twice → same record count, second
// run all Updated=true, changed source values land.
func TestCommitRowsUpserting_SecondRunUpdatesFirstRunsRecords(t *testing.T) {
	headers, mapping := itemImportShape()
	eng := &fakeEngine{}
	def := purchasing.Item()
	const rel = "dbo.A$Item"

	first := upsert(t, eng, def, headers, mapping,
		[][]string{{"1000", "Bicycle", "stock"}, {"1002", "Chain", "stock"}}, "No_", "src-1", rel)
	for _, res := range first {
		if res.Err != nil || res.Updated {
			t.Fatalf("first run: expected creates only, got %+v", res)
		}
	}

	second := upsert(t, eng, def, headers, mapping,
		[][]string{{"1000", "Bicycle Deluxe", "stock"}, {"1002", "Chain", "stock"}}, "No_", "src-1", rel)
	for i, res := range second {
		if res.Err != nil || !res.Updated {
			t.Fatalf("second run row %d: expected an update, got %+v", i+1, res)
		}
		if res.RecordID != first[i].RecordID {
			t.Fatalf("second run row %d updated %s, expected the first run's record %s", i+1, res.RecordID, first[i].RecordID)
		}
	}
	if got := len(eng.ofType("Item")); got != 2 {
		t.Fatalf("re-import must not duplicate: expected 2 Item records, got %d", got)
	}
	if got := len(eng.ofType("ExternalIdentity")); got != 2 {
		t.Fatalf("re-import must not duplicate identities: expected 2, got %d", got)
	}
	var found bool
	for _, rec := range eng.ofType("Item") {
		if rec.Data["sku"] == "1000" {
			found = true
			if rec.Data["name"] != "Bicycle Deluxe" {
				t.Fatalf("expected the changed Description to land, got %v", rec.Data["name"])
			}
		}
	}
	if !found {
		t.Fatal("record with sku 1000 disappeared")
	}
}

// TestCommitRowsUpserting_RelationIsPartOfIdentityScope is the review's
// headline defect made a test: NAV's $Customer and $Vendor both land in
// Party and their number series overlap — Customer 10000 and Vendor
// 10000 are DIFFERENT companies. The second relation's import must
// create a new record, never update the first relation's.
func TestCommitRowsUpserting_RelationIsPartOfIdentityScope(t *testing.T) {
	headers, mapping := partyImportShape()
	eng := &fakeEngine{}
	def := foundation.Party()

	customers := upsert(t, eng, def, headers, mapping,
		[][]string{{"10000", "Adatum Corporation", "organization"}}, "No_", "src-1", "dbo.A$Customer")
	if customers[0].Err != nil || customers[0].Updated {
		t.Fatalf("customer import: expected a create, got %+v", customers[0])
	}

	// Same source, same entity type, same key "10000" — different relation.
	vendors := upsert(t, eng, def, headers, mapping,
		[][]string{{"10000", "London Postmaster", "organization"}}, "No_", "src-1", "dbo.A$Vendor")
	if vendors[0].Err != nil || vendors[0].Updated {
		t.Fatalf("vendor import must CREATE, not update the customer's record: %+v", vendors[0])
	}
	if vendors[0].RecordID == customers[0].RecordID {
		t.Fatal("vendor 10000 landed on customer 10000's record — relation is not scoping the identity")
	}
	if got := len(eng.ofType("Party")); got != 2 {
		t.Fatalf("expected 2 Party records (customer + vendor), got %d", got)
	}
	// And the customer record kept its own name.
	cust, err := eng.Get(context.Background(), def, customers[0].RecordID)
	if err != nil {
		t.Fatal(err)
	}
	if cust.Data["name"] != "Adatum Corporation" {
		t.Fatalf("customer record was overwritten by the vendor import: %v", cust.Data["name"])
	}

	// Re-running each relation still updates its OWN record.
	customers2 := upsert(t, eng, def, headers, mapping,
		[][]string{{"10000", "Adatum Corp (renamed)", "organization"}}, "No_", "src-1", "dbo.A$Customer")
	if !customers2[0].Updated || customers2[0].RecordID != customers[0].RecordID {
		t.Fatalf("customer re-import should update the customer record, got %+v", customers2[0])
	}
	if got := len(eng.ofType("Party")); got != 2 {
		t.Fatalf("expected the Party count to stay 2, got %d", got)
	}
}

// TestCommitRowsUpserting_IdentityIsScopedPerSource — the same relation
// name and key from a DIFFERENT registered source (a NAV mirror, say) is
// a different identity: it must create, not update.
func TestCommitRowsUpserting_IdentityIsScopedPerSource(t *testing.T) {
	headers, mapping := itemImportShape()
	eng := &fakeEngine{}
	def := purchasing.Item()
	const rel = "dbo.A$Item"

	if res := upsert(t, eng, def, headers, mapping,
		[][]string{{"1000", "Bicycle", "stock"}}, "No_", "src-1", rel); res[0].Err != nil {
		t.Fatal(res[0].Err)
	}
	// Same key "1000", same relation name — different registered source.
	results := upsert(t, eng, def, headers, mapping,
		[][]string{{"1000", "Mirror Bicycle", "stock"}}, "No_", "src-2", rel)
	if results[0].Err != nil || results[0].Updated {
		t.Fatalf("a different source's key must create, not update: %+v", results[0])
	}
	if got := len(eng.ofType("Item")); got != 2 {
		t.Fatalf("expected 2 Item records (one per source), got %d", got)
	}
}

// TestCommitRowsUpserting_UpdateMergesUnmappedFields pins the merge
// semantics the review demanded: crud.Engine.Update is a full
// replacement, so a refresh overlaying only the mapped fields onto the
// stored record is what keeps hand-set / unmapped fields alive.
func TestCommitRowsUpserting_UpdateMergesUnmappedFields(t *testing.T) {
	headers, mapping := partyImportShape()
	eng := &fakeEngine{}
	def := foundation.Party()
	const rel = "dbo.A$Customer"

	first := upsert(t, eng, def, headers, mapping,
		[][]string{{"C001", "Acme Ltd", "organization"}}, "No_", "src-1", rel)
	if first[0].Err != nil {
		t.Fatal(first[0].Err)
	}
	// A human sets a field the mapping doesn't carry (tax_id has no NAV
	// column in this shape).
	for i, rec := range eng.records {
		if rec.EntityType == "Party" && rec.ID == first[0].RecordID {
			eng.records[i].Data["tax_id"] = "TR-12345"
		}
	}

	second := upsert(t, eng, def, headers, mapping,
		[][]string{{"C001", "Acme Holdings Ltd", "organization"}}, "No_", "src-1", rel)
	if second[0].Err != nil || !second[0].Updated {
		t.Fatalf("expected an update, got %+v", second[0])
	}
	rec, err := eng.Get(context.Background(), def, first[0].RecordID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Data["name"] != "Acme Holdings Ltd" {
		t.Fatalf("mapped field should be refreshed, got %v", rec.Data["name"])
	}
	if rec.Data["tax_id"] != "TR-12345" {
		t.Fatalf("unmapped hand-set field must survive the refresh, got %v", rec.Data["tax_id"])
	}
}

// TestCommitRowsUpserting_DuplicateKeyInRunIsRowError — a non-unique key
// column (a legacy view, a bad join) must not silently collapse rows
// with the last one winning: the first row creates, the second gets a
// row error naming the first.
func TestCommitRowsUpserting_DuplicateKeyInRunIsRowError(t *testing.T) {
	headers, mapping := itemImportShape()
	eng := &fakeEngine{}
	results := upsert(t, eng, purchasing.Item(), headers, mapping,
		[][]string{{"1000", "Bicycle", "stock"}, {"1000", "Bicycle Copy", "stock"}},
		"No_", "src-1", "dbo.A$Item")
	if results[0].Err != nil || results[0].RecordID == "" {
		t.Fatalf("first use of the key should create, got %+v", results[0])
	}
	if results[1].Err == nil || !strings.Contains(results[1].Err.Error(), "duplicate key") ||
		!strings.Contains(results[1].Err.Error(), "row 1") {
		t.Fatalf("expected a duplicate-key error naming row 1, got %+v", results[1])
	}
	if results[1].RecordID != "" || results[1].Updated {
		t.Fatalf("the duplicate row must not be written, got %+v", results[1])
	}
	if got := len(eng.ofType("Item")); got != 1 {
		t.Fatalf("expected exactly 1 Item record, got %d", got)
	}
	// The first row's data won — not the duplicate's.
	if name := eng.ofType("Item")[0].Data["name"]; name != "Bicycle" {
		t.Fatalf("the earlier row's record was overwritten by the duplicate: %v", name)
	}
}

func TestCommitRowsUpserting_RecordCreateFailureIsRowError(t *testing.T) {
	headers, mapping := itemImportShape()
	boom := errors.New("records engine down")
	records := &fakeEngine{failCreate: boom}
	identities := &fakeEngine{}
	results, err := CommitRowsUpserting(context.Background(), headers,
		[][]string{{"1000", "Bicycle", "stock"}}, purchasing.Item(), mapping,
		"No_", "src-1", "dbo.A$Item", records, identities, foundation.ExternalIdentity(), upsertTestActor)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(results[0].Err, boom) || results[0].RecordID != "" {
		t.Fatalf("expected the create failure as a row error with no RecordID, got %+v", results[0])
	}
	if got := len(identities.ofType("ExternalIdentity")); got != 0 {
		t.Fatalf("no identity may be written for a record that never landed, got %d", got)
	}
}

// TestCommitRowsUpserting_IdentityCreateFailureReportsReRunDuplication —
// the record landed but its identity didn't: the row must carry BOTH the
// RecordID (the record exists) and an error that says a re-run will
// duplicate it once (the sequencing note made visible).
func TestCommitRowsUpserting_IdentityCreateFailureReportsReRunDuplication(t *testing.T) {
	headers, mapping := itemImportShape()
	records := &fakeEngine{}
	identities := &fakeEngine{failCreate: errors.New("identity write refused")}
	results, err := CommitRowsUpserting(context.Background(), headers,
		[][]string{{"1000", "Bicycle", "stock"}}, purchasing.Item(), mapping,
		"No_", "src-1", "dbo.A$Item", records, identities, foundation.ExternalIdentity(), upsertTestActor)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].RecordID == "" {
		t.Fatalf("the record WAS created — RecordID must be set, got %+v", results[0])
	}
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "re-run will duplicate") {
		t.Fatalf("expected an error naming the re-run duplication consequence, got %v", results[0].Err)
	}
	if got := len(records.ofType("Item")); got != 1 {
		t.Fatalf("expected the record to exist, got %d", got)
	}
	if got := len(identities.ofType("ExternalIdentity")); got != 0 {
		t.Fatalf("the identity write failed — none may exist, got %d", got)
	}
}

func TestCommitRowsUpserting_GetFailureOnUpdatePathIsRowError(t *testing.T) {
	headers, mapping := itemImportShape()
	boom := errors.New("get refused")
	records := &fakeEngine{failGet: boom}
	identities := &fakeEngine{}
	identities.seedIdentity("src-1", "dbo.A$Item", "Item", "rec-existing", "1000")

	results, err := CommitRowsUpserting(context.Background(), headers,
		[][]string{{"1000", "Bicycle", "stock"}}, purchasing.Item(), mapping,
		"No_", "src-1", "dbo.A$Item", records, identities, foundation.ExternalIdentity(), upsertTestActor)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(results[0].Err, boom) || results[0].Updated || results[0].RecordID != "" {
		t.Fatalf("expected the merge-read failure as a row error, got %+v", results[0])
	}
	if records.updates != 0 {
		t.Fatalf("no update may run when the merge read failed, got %d", records.updates)
	}
}

func TestCommitRowsUpserting_UpdateFailureIsRowError(t *testing.T) {
	headers, mapping := itemImportShape()
	boom := errors.New("update refused")
	records := &fakeEngine{failUpdate: boom}
	identities := &fakeEngine{}
	// A real stored record for the merge read, plus its identity.
	stored, err := records.Create(context.Background(), purchasing.Item(),
		map[string]any{"sku": "1000", "name": "Bicycle", "item_type": "stock"}, upsertTestActor)
	if err != nil {
		t.Fatal(err)
	}
	identities.seedIdentity("src-1", "dbo.A$Item", "Item", stored.ID, "1000")

	results, err := CommitRowsUpserting(context.Background(), headers,
		[][]string{{"1000", "Bicycle Deluxe", "stock"}}, purchasing.Item(), mapping,
		"No_", "src-1", "dbo.A$Item", records, identities, foundation.ExternalIdentity(), upsertTestActor)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(results[0].Err, boom) || results[0].Updated || results[0].RecordID != "" {
		t.Fatalf("expected the update failure as a row error, got %+v", results[0])
	}
}

func TestCommitRowsUpserting_IdentityPreloadFailureFailsTheRun(t *testing.T) {
	headers, mapping := itemImportShape()
	records := &fakeEngine{}
	identities := &fakeEngine{failList: errors.New("identities unreadable")}
	_, err := CommitRowsUpserting(context.Background(), headers,
		[][]string{{"1000", "Bicycle", "stock"}}, purchasing.Item(), mapping,
		"No_", "src-1", "dbo.A$Item", records, identities, foundation.ExternalIdentity(), upsertTestActor)
	if err == nil || !strings.Contains(err.Error(), "load identities") {
		t.Fatalf("a failed identity pre-load must fail the whole run (no blind creates), got %v", err)
	}
	if len(records.records) != 0 {
		t.Fatalf("nothing may be written when the pre-load failed, got %d records", len(records.records))
	}
}

func TestCommitRowsUpserting_IdentityWithoutRecordIDIsRowError(t *testing.T) {
	headers, mapping := itemImportShape()
	eng := &fakeEngine{}
	eng.seedIdentity("src-1", "dbo.A$Item", "Item", "", "1000") // corrupt: no record_id

	results := upsert(t, eng, purchasing.Item(), headers, mapping,
		[][]string{{"1000", "Bicycle", "stock"}}, "No_", "src-1", "dbo.A$Item")
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "no record_id") {
		t.Fatalf("expected a no-record_id row error, got %+v", results[0])
	}
	if results[0].RecordID != "" || results[0].Updated {
		t.Fatalf("a corrupt identity must not be written through, got %+v", results[0])
	}
	if got := len(eng.ofType("Item")); got != 0 {
		t.Fatalf("a corrupt identity must not fall back to create, got %d", got)
	}
}

// TestCommitRowsUpserting_CancelledContextMarksRowsNotAttempted mirrors
// csvimport.CommitRows' context contract: rows never attempted carry the
// context error rather than being silently dropped or half-tried.
func TestCommitRowsUpserting_CancelledContextMarksRowsNotAttempted(t *testing.T) {
	headers, mapping := itemImportShape()
	eng := &fakeEngine{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results, err := CommitRowsUpserting(ctx, headers, [][]string{{"1000", "Bicycle", "stock"}},
		purchasing.Item(), mapping, "No_", "src-1", "dbo.A$Item", eng, eng,
		foundation.ExternalIdentity(), upsertTestActor)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "not attempted") {
		t.Fatalf("expected a not-attempted context error, got %+v", results[0])
	}
	if len(eng.records) != 0 {
		t.Fatalf("nothing should be written under a cancelled context, got %d records", len(eng.records))
	}
}

// TestCommitRowsUpserting_InvalidRowIsSkippedNotWritten — csvimport's
// per-row validation contract carries through: a row failing entity
// validation gets its preview error, no lookup, no write.
func TestCommitRowsUpserting_InvalidRowIsSkippedNotWritten(t *testing.T) {
	headers, mapping := itemImportShape()
	rows := [][]string{
		{"1000", "", "stock"}, // blank Description → required name absent
		{"1002", "Chain", "stock"},
	}
	eng := &fakeEngine{}
	results := upsert(t, eng, purchasing.Item(), headers, mapping, rows, "No_", "src-1", "dbo.A$Item")
	if results[0].Err == nil || results[0].RecordID != "" {
		t.Fatalf("expected the invalid row skipped with its validation error, got %+v", results[0])
	}
	if results[1].Err != nil || results[1].RecordID == "" {
		t.Fatalf("expected the valid row to commit, got %+v", results[1])
	}
	if got := len(eng.ofType("Item")); got != 1 {
		t.Fatalf("expected exactly 1 Item record, got %d", got)
	}
}
