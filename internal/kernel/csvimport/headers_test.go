package csvimport

import (
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/entity"
)

func TestHeaders_ReadsHeaderRowOnly(t *testing.T) {
	csvData := "Vendor Name,Lead Time\nAcme,60\nBeta,45\n"
	headers, err := Headers(strings.NewReader(csvData))
	if err != nil {
		t.Fatalf("Headers: %v", err)
	}
	if len(headers) != 2 || headers[0] != "Vendor Name" || headers[1] != "Lead Time" {
		t.Fatalf("unexpected headers: %+v", headers)
	}
}

func TestHeaders_StripsUTF8BOM(t *testing.T) {
	// Same footgun Preview already guards against (readCSV is shared) —
	// Headers must see it too, not just Preview.
	csvData := "\uFEFFVendor Name,Lead Time\nAcme,60\n"
	headers, err := Headers(strings.NewReader(csvData))
	if err != nil {
		t.Fatalf("Headers: %v", err)
	}
	if headers[0] != "Vendor Name" {
		t.Fatalf("expected BOM stripped from first header, got %q", headers[0])
	}
}

func TestHeaders_EmptyFileIsAnError(t *testing.T) {
	_, err := Headers(strings.NewReader(""))
	if err == nil {
		t.Fatal("expected an error for a CSV with no header row")
	}
}

func vendorDefForHeaders() *entity.Definition {
	return &entity.Definition{
		EntityType: "Vendor",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "lead_time_days", Type: entity.FieldNumber},
			{Name: "is_active", Type: entity.FieldBool},
		},
	}
}

func TestSuggestMapping_MatchesNormalizedNames(t *testing.T) {
	headers := []string{"Name", "Lead Time Days", "is-active", "Unrelated Column"}
	got := SuggestMapping(headers, vendorDefForHeaders())

	want := ColumnMapping{
		"Name":           "name",
		"Lead Time Days": "lead_time_days",
		"is-active":      "is_active",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d mapped columns, got %d: %+v", len(want), len(got), got)
	}
	for header, field := range want {
		if got[header] != field {
			t.Fatalf("expected %q to map to %q, got %q", header, field, got[header])
		}
	}
	if _, mapped := got["Unrelated Column"]; mapped {
		t.Fatalf("expected a header with no matching field to stay unmapped, got %+v", got)
	}
}

func TestSuggestMapping_ExactCaseSensitiveNameStillMatches(t *testing.T) {
	got := SuggestMapping([]string{"name"}, vendorDefForHeaders())
	if got["name"] != "name" {
		t.Fatalf("expected an exact-name header to map to itself, got %+v", got)
	}
}

// TestSuggestMapping_FirstDuplicateHeaderWinsNotBoth is the regression
// test for the exact nondeterministic-clobbering bug ValidateMapping was
// hardened against for hand-written mappings (see csvimport_test.go's
// TestValidateMapping_RejectsDuplicateTargetField): SuggestMapping must
// never itself PRODUCE a mapping with two headers claiming the same
// field, which ValidateMapping would then reject outright.
func TestSuggestMapping_FirstDuplicateHeaderWinsNotBoth(t *testing.T) {
	headers := []string{"Name", "name", "NAME"} // all normalize to the same field
	got := SuggestMapping(headers, vendorDefForHeaders())

	mappedCount := 0
	for _, field := range got {
		if field == "name" {
			mappedCount++
		}
	}
	if mappedCount != 1 {
		t.Fatalf("expected exactly one header to claim the name field, got %d claims in %+v", mappedCount, got)
	}
	if got["Name"] != "name" {
		t.Fatalf("expected the first-occurring header (%q) to win, got %+v", "Name", got)
	}

	// The suggested mapping must itself pass ValidateMapping — proving
	// this isn't just an assertion about SuggestMapping's output shape,
	// but that it produces something Preview/Commit would actually accept.
	if err := ValidateMapping(vendorDefForHeaders(), headers, got); err != nil {
		t.Fatalf("expected SuggestMapping's own output to pass ValidateMapping, got %v", err)
	}
}

func TestSuggestMapping_NoMatchesReturnsEmptyMapping(t *testing.T) {
	got := SuggestMapping([]string{"Totally Unrelated"}, vendorDefForHeaders())
	if len(got) != 0 {
		t.Fatalf("expected an empty mapping when nothing matches, got %+v", got)
	}
}
