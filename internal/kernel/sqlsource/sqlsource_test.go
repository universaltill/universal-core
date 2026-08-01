package sqlsource

import (
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/csvimport"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

func TestDSN_MSSQL(t *testing.T) {
	s := Source{
		Name: "nav", Driver: "mssql", Host: "nav.internal",
		Database: "NAVDB", Username: "reader", Password: "s3cret",
	}
	dsn, err := s.DSN()
	if err != nil {
		t.Fatal(err)
	}
	want := "sqlserver://reader:s3cret@nav.internal:1433?database=NAVDB"
	if dsn != want {
		t.Fatalf("got %q, want %q", dsn, want)
	}
}

func TestDSN_MSSQLDefaultPortIs1433(t *testing.T) {
	s := Source{Name: "nav", Driver: "mssql", Host: "h", Database: "d"}
	dsn, err := s.DSN()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "h:1433") {
		t.Fatalf("expected default mssql port 1433 in %q", dsn)
	}
}

func TestDSN_Postgres(t *testing.T) {
	s := Source{
		Name: "mirror", Driver: "postgres", Host: "db.internal", Port: 5433,
		Database: "navmirror", Username: "ro", Password: "pw",
	}
	dsn, err := s.DSN()
	if err != nil {
		t.Fatal(err)
	}
	want := "postgres://ro:pw@db.internal:5433/navmirror"
	if dsn != want {
		t.Fatalf("got %q, want %q", dsn, want)
	}
}

// TestDSN_PasswordWithSpecialCharactersSurvives — a legacy-ERP service
// account's password full of URL metacharacters must round-trip via
// proper escaping, not string concatenation.
func TestDSN_PasswordWithSpecialCharactersSurvives(t *testing.T) {
	s := Source{
		Name: "nav", Driver: "postgres", Host: "h", Database: "d",
		Username: "u", Password: "p@ss:w/ord?&=%",
	}
	dsn, err := s.DSN()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dsn, "p@ss:w/ord?&=%") {
		t.Fatalf("password appears unescaped in DSN %q", dsn)
	}
	if !strings.Contains(dsn, "u:") || !strings.Contains(dsn, "@h:5432") {
		t.Fatalf("unexpected DSN shape %q", dsn)
	}
}

func TestDSN_OptionsArePreserved(t *testing.T) {
	s := Source{
		Name: "nav", Driver: "mssql", Host: "h", Database: "d",
		Options: "encrypt=disable",
	}
	dsn, err := s.DSN()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "encrypt=disable") || !strings.Contains(dsn, "database=d") {
		t.Fatalf("expected options and database in DSN, got %q", dsn)
	}
}

func TestDSN_RejectsBadOptionsSyntax(t *testing.T) {
	s := Source{Name: "x", Driver: "postgres", Host: "h", Database: "d", Options: "a=%zz"}
	if _, err := s.DSN(); err == nil {
		t.Fatal("expected an error for malformed options query syntax")
	}
}

func TestDSN_RejectsUnknownDriver(t *testing.T) {
	s := Source{Name: "x", Driver: "oracle", Host: "h", Database: "d"}
	if _, err := s.DSN(); err == nil {
		t.Fatal("expected an error for an unsupported driver")
	}
}

func TestApplyConstants_InjectsSyntheticColumns(t *testing.T) {
	headers := []string{"No_", "Description"}
	rows := [][]string{{"1000", "Bicycle"}, {"1001", "Wheel"}}
	mapping := csvimport.ColumnMapping{"No_": "sku", "Description": "name"}

	h, r, m, err := ApplyConstants(headers, rows, mapping, map[string]string{"item_type": "stock"})
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 3 || h[2] != "__const:item_type" {
		t.Fatalf("expected appended constant column, got %v", h)
	}
	for i, row := range r {
		if len(row) != 3 || row[2] != "stock" {
			t.Fatalf("row %d: expected constant value appended, got %v", i, row)
		}
	}
	if m["__const:item_type"] != "item_type" {
		t.Fatalf("expected mapping entry for constant column, got %v", m)
	}
	if len(m) != 3 {
		t.Fatalf("expected original mapping preserved plus one constant, got %v", m)
	}
}

func TestApplyConstants_DoesNotMutateInputs(t *testing.T) {
	headers := []string{"A"}
	rows := [][]string{{"x"}}
	mapping := csvimport.ColumnMapping{"A": "name"}

	_, _, _, err := ApplyConstants(headers, rows, mapping, map[string]string{"party_type": "organization"})
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 1 || len(rows[0]) != 1 || len(mapping) != 1 {
		t.Fatalf("inputs were mutated: headers=%v rows=%v mapping=%v", headers, rows, mapping)
	}
}

func TestApplyConstants_MultipleConstantsDeterministicOrder(t *testing.T) {
	h1, _, _, err := ApplyConstants([]string{"A"}, [][]string{{"x"}}, csvimport.ColumnMapping{}, map[string]string{"b_field": "2", "a_field": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if h1[1] != "__const:a_field" || h1[2] != "__const:b_field" {
		t.Fatalf("expected constants sorted by field name, got %v", h1)
	}
}

func TestApplyConstants_RejectsReservedPrefixCollision(t *testing.T) {
	_, _, _, err := ApplyConstants([]string{"__const:x"}, nil, csvimport.ColumnMapping{}, map[string]string{"f": "v"})
	if err == nil {
		t.Fatal("expected an error for a source column using the reserved prefix")
	}
}

// TestApplyConstants_EndToEndThroughPreviewRows proves the injected
// constants actually satisfy a required enum field the source columns
// cannot — the exact NAV case the mechanism exists for.
func TestApplyConstants_EndToEndThroughPreviewRows(t *testing.T) {
	def := purchasing.Item()
	headers := []string{"No_", "Description"}
	rows := [][]string{{"1000", "Bicycle"}}
	mapping := csvimport.ColumnMapping{"No_": "sku", "Description": "name"}

	// Without the constant, ValidateMapping must reject: required
	// item_type has no mapped column.
	if _, err := csvimport.PreviewRows(headers, rows, def, mapping); err == nil {
		t.Fatal("expected ValidateMapping to reject a mapping missing required item_type")
	}

	h, r, m, err := ApplyConstants(headers, rows, mapping, map[string]string{"item_type": "stock"})
	if err != nil {
		t.Fatal(err)
	}
	results, err := csvimport.PreviewRows(h, r, def, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("expected one valid row, got %+v", results)
	}
	if results[0].Data["item_type"] != "stock" || results[0].Data["sku"] != "1000" {
		t.Fatalf("unexpected row data %v", results[0].Data)
	}
}

// TestNAV2009TemplateTargetsRealEntitiesAndFields pins the template
// (pure data) against the actual entity definitions it claims to map
// onto — a renamed field or entity breaks this test, not a tenant's
// first import.
func TestNAV2009TemplateTargetsRealEntitiesAndFields(t *testing.T) {
	defs := map[string]*entity.Definition{}
	for _, d := range foundation.All() {
		defs[d.EntityType] = d
	}
	defs["Item"] = purchasing.Item()

	for _, te := range nav2009Template().Entities {
		def, ok := defs[te.EntityType]
		if !ok {
			t.Errorf("template %q targets unknown entity type %q", te.RelationSuffix, te.EntityType)
			continue
		}
		for col, field := range te.Mapping {
			if _, ok := def.FieldByName(field); !ok {
				t.Errorf("template %q maps column %q to %q.%q, which doesn't exist", te.RelationSuffix, col, te.EntityType, field)
			}
		}
		for field, raw := range te.Constants {
			f, ok := def.FieldByName(field)
			if !ok {
				t.Errorf("template %q constant targets %q.%q, which doesn't exist", te.RelationSuffix, te.EntityType, field)
				continue
			}
			if _, err := csvimport.Coerce(f.Type, raw); err != nil {
				t.Errorf("template %q constant %q=%q doesn't coerce to field type: %v", te.RelationSuffix, field, raw, err)
			}
		}
	}
}

func TestMatchTemplate_SuffixMatchesAnyCompanyName(t *testing.T) {
	tpl, te, ok := MatchTemplate("CRONUS International Ltd_$Item")
	if !ok {
		t.Fatal("expected a template match for a NAV per-company Item table")
	}
	if tpl.Key != "nav2009" || te.EntityType != "Item" {
		t.Fatalf("unexpected match %q/%q", tpl.Key, te.EntityType)
	}
	if _, _, ok := MatchTemplate("some_random_table"); ok {
		t.Fatal("expected no match for a non-NAV relation name")
	}
}

// TestDSN_DeniesFileAndIdentityOptions — options that would make the
// driver read files off OUR server (passfile/sslkey exfiltration
// against an attacker-chosen host) or override the stored record's
// identity/target must be rejected at DSN build time.
func TestDSN_DeniesFileAndIdentityOptions(t *testing.T) {
	for _, opt := range []string{
		"passfile=/etc/passwd", "sslkey=/tmp/k", "sslcert=/tmp/c",
		"user=admin", "password=x", "host=elsewhere", "database=other",
		"Passfile=/tmp/p", // case-insensitive
	} {
		s := Source{Name: "x", Driver: "postgres", Host: "h", Database: "d", Options: opt}
		if _, err := s.DSN(); err == nil {
			t.Errorf("expected option %q to be rejected", opt)
		}
	}
	// Harmless driver options still pass.
	s := Source{Name: "x", Driver: "postgres", Host: "h", Database: "d", Options: "connect_timeout=5"}
	if _, err := s.DSN(); err != nil {
		t.Errorf("expected connect_timeout to be allowed, got %v", err)
	}
}

// TestDriverEnumStaysInSyncWithDataLayer pins the three places the
// driver enum is declared (ExternalSQLSource's EnumValues, the data
// layer's ExtDriver* constants, DSN()'s switch) to one another.
func TestDriverEnumStaysInSyncWithDataLayer(t *testing.T) {
	def := foundation.ExternalSQLSource()
	f, ok := def.FieldByName("driver")
	if !ok {
		t.Fatal("ExternalSQLSource has no driver field")
	}
	want := map[string]bool{data.ExtDriverMSSQL: true, data.ExtDriverPostgres: true}
	if len(f.EnumValues) != len(want) {
		t.Fatalf("driver enum %v does not match data layer drivers %v", f.EnumValues, want)
	}
	for _, v := range f.EnumValues {
		if !want[v] {
			t.Errorf("driver enum value %q has no matching data.ExtDriver constant", v)
		}
		s := Source{Name: "x", Driver: v, Host: "h", Database: "d"}
		if _, err := s.DSN(); err != nil {
			t.Errorf("DSN() cannot build for declared driver %q: %v", v, err)
		}
	}
}
