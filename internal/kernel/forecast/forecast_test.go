package forecast

import (
	"math"
	"testing"
	"time"
)

// approxEqual sidesteps float64 representation noise (0.1+0.2 != 0.3) —
// quantile interpolation multiplies and adds fractions, so exact ==
// would make these tests flaky against harmless last-bit differences.
func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestQuantile(t *testing.T) {
	tests := []struct {
		name    string
		samples []float64
		q       float64
		want    float64
		wantOK  bool
	}{
		{"empty input has no quantile", nil, 0.5, 0, false},
		{"q below range", []float64{1, 2}, -0.1, 0, false},
		{"q above range", []float64{1, 2}, 1.1, 0, false},
		// NaN slips through plain range comparisons (NaN < 0 and NaN > 1
		// are both false) and int(math.Floor(NaN)) is platform-divergent —
		// the guard must catch it explicitly.
		{"q NaN is rejected", []float64{1, 2}, math.NaN(), 0, false},
		{"q +Inf is rejected", []float64{1, 2}, math.Inf(1), 0, false},
		{"q -Inf is rejected", []float64{1, 2}, math.Inf(-1), 0, false},
		{"single sample is its own median", []float64{7}, 0.5, 7, true},
		{"single sample is its own p90", []float64{7}, 0.9, 7, true},
		{"two samples median interpolates halfway", []float64{9, 11}, 0.5, 10, true},
		{"two samples p90 interpolates", []float64{9, 11}, 0.9, 10.8, true},
		{"min is q=0", []float64{3, 1, 2}, 0, 1, true},
		{"max is q=1", []float64{3, 1, 2}, 1, 3, true},
		// Type-7 worked example: n=4 sorted [10,20,30,40], h=(4-1)*0.5=1.5
		// -> 20 + 0.5*(30-20) = 25.
		{"four samples median interpolates between middle pair", []float64{10, 20, 30, 40}, 0.5, 25, true},
		// h=(4-1)*0.9=2.7 -> 30 + 0.7*(40-30) = 37.
		{"four samples p90", []float64{10, 20, 30, 40}, 0.9, 37, true},
		// Exact order statistic (no interpolation) when h is integral:
		// n=5, h=(5-1)*0.5=2 -> sorted[2] = 30.
		{"odd count median is the middle value exactly", []float64{50, 10, 30, 20, 40}, 0.5, 30, true},
		// Unsorted input must give the identical answer as sorted input —
		// Quantile sorts internally (see its doc comment).
		{"unsorted input is sorted internally", []float64{40, 10, 30, 20}, 0.5, 25, true},
		{"duplicates are legal observations", []float64{5, 5, 5, 5}, 0.9, 5, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Quantile(tc.samples, tc.q)
			if ok != tc.wantOK {
				t.Fatalf("Quantile(%v, %v) ok = %v, want %v", tc.samples, tc.q, ok, tc.wantOK)
			}
			if ok && !approxEqual(got, tc.want) {
				t.Fatalf("Quantile(%v, %v) = %v, want %v", tc.samples, tc.q, got, tc.want)
			}
		})
	}
}

func TestFires(t *testing.T) {
	tests := []struct {
		name                                string
		position, reorderPoint, safetyStock float64
		want                                bool
	}{
		{"well below threshold fires", 10, 100, 0, true},
		{"exactly at threshold fires (inclusive boundary)", 100, 100, 0, true},
		{"exactly at threshold with safety stock fires", 150, 100, 50, true},
		{"one above threshold does not fire", 151, 100, 50, false},
		{"safety stock raises the threshold", 120, 100, 50, true},
		{"zero position fires", 0, 1, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Fires(tc.position, tc.reorderPoint, tc.safetyStock); got != tc.want {
				t.Fatalf("Fires(%v, %v, %v) = %v, want %v", tc.position, tc.reorderPoint, tc.safetyStock, got, tc.want)
			}
		})
	}
}

func TestQuantile_DoesNotMutateCallerSlice(t *testing.T) {
	samples := []float64{3, 1, 2}
	if _, ok := Quantile(samples, 0.5); !ok {
		t.Fatal("expected ok")
	}
	if samples[0] != 3 || samples[1] != 1 || samples[2] != 2 {
		t.Fatalf("caller's slice was mutated: %v", samples)
	}
}

func day(d string) time.Time {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		panic(err)
	}
	return t
}

func sample(vendor, ordered, received string) LeadTimeSample {
	return LeadTimeSample{VendorID: vendor, OrderDate: day(ordered), ReceivedDate: day(received)}
}

func TestLeadTimeSample_LeadTimeDays(t *testing.T) {
	s := sample("v1", "2026-07-18", "2026-07-27")
	if got := s.LeadTimeDays(); !approxEqual(got, 9) {
		t.Fatalf("LeadTimeDays = %v, want 9", got)
	}
}

func TestCompute_OverallAndPerVendor(t *testing.T) {
	// Vendor A: 9 and 11 days -> P50 10, P90 10.8. Vendor B: one 20-day
	// order -> N=1, insufficient. Overall: [9, 11, 20] -> P50 11,
	// P90 = 11 + 0.8*(20-11) = 18.2 (h = 2*0.9 = 1.8).
	samples := []LeadTimeSample{
		sample("vendor-a", "2026-07-18", "2026-07-27"),
		sample("vendor-a", "2026-07-01", "2026-07-12"),
		sample("vendor-b", "2026-06-01", "2026-06-21"),
	}
	got := Compute(samples)

	if got.Overall.N != 3 || !got.Overall.Sufficient() {
		t.Fatalf("Overall.N = %d (sufficient=%v), want 3 sufficient", got.Overall.N, got.Overall.Sufficient())
	}
	if !approxEqual(got.Overall.P50Days, 11) || !approxEqual(got.Overall.P90Days, 18.2) {
		t.Fatalf("Overall P50/P90 = %v/%v, want 11/18.2", got.Overall.P50Days, got.Overall.P90Days)
	}

	a := got.ByVendor["vendor-a"]
	if a.N != 2 || !a.Sufficient() {
		t.Fatalf("vendor-a N = %d (sufficient=%v), want 2 sufficient", a.N, a.Sufficient())
	}
	if !approxEqual(a.P50Days, 10) || !approxEqual(a.P90Days, 10.8) {
		t.Fatalf("vendor-a P50/P90 = %v/%v, want 10/10.8", a.P50Days, a.P90Days)
	}

	b := got.ByVendor["vendor-b"]
	if b.N != 1 {
		t.Fatalf("vendor-b N = %d, want 1", b.N)
	}
	if b.Sufficient() {
		t.Fatal("vendor-b with one sample must be insufficient — never a fabricated quantile")
	}
	if b.P50Days != 0 || b.P90Days != 0 {
		t.Fatalf("insufficient vendor must carry zero quantiles, got %v/%v", b.P50Days, b.P90Days)
	}
}

func TestCompute_EmptyAndSingleSample(t *testing.T) {
	empty := Compute(nil)
	if empty.Overall.N != 0 || empty.Overall.Sufficient() {
		t.Fatalf("empty input: Overall = %+v, want N=0 insufficient", empty.Overall)
	}
	if len(empty.ByVendor) != 0 {
		t.Fatalf("empty input: ByVendor = %v, want empty", empty.ByVendor)
	}

	one := Compute([]LeadTimeSample{sample("v1", "2026-07-01", "2026-07-10")})
	if one.Overall.N != 1 || one.Overall.Sufficient() {
		t.Fatalf("single sample: Overall = %+v, want N=1 insufficient", one.Overall)
	}
}

// TestCompute_SkipsInvalidSamples: stage dates are noisy user input
// (issue #29's review note — imported dates bypass chronology
// validation entirely), so a reversed or half-empty sample must be
// dropped, not averaged in as a negative lead time.
func TestCompute_SkipsInvalidSamples(t *testing.T) {
	samples := []LeadTimeSample{
		sample("v1", "2026-07-01", "2026-07-11"),
		sample("v1", "2026-07-05", "2026-07-13"),
		// Received BEFORE ordered — reversed noise, must be skipped.
		sample("v1", "2026-07-20", "2026-07-10"),
		// Missing endpoints — must be skipped.
		{VendorID: "v1", ReceivedDate: day("2026-07-10")},
		{VendorID: "v1", OrderDate: day("2026-07-10")},
	}
	got := Compute(samples)
	v := got.ByVendor["v1"]
	if v.N != 2 {
		t.Fatalf("N = %d, want 2 (invalid samples must not count)", v.N)
	}
	// [10, 8] -> P50 9.
	if !approxEqual(v.P50Days, 9) {
		t.Fatalf("P50 = %v, want 9", v.P50Days)
	}
}

// TestCompute_VendorlessSamplesCountOnlyOverall: a sample whose PO has
// no resolvable vendor still informs the overall distribution but can't
// be attributed to any vendor bucket.
func TestCompute_VendorlessSamplesCountOnlyOverall(t *testing.T) {
	samples := []LeadTimeSample{
		sample("", "2026-07-01", "2026-07-11"),
		sample("", "2026-07-02", "2026-07-14"),
	}
	got := Compute(samples)
	if got.Overall.N != 2 {
		t.Fatalf("Overall.N = %d, want 2", got.Overall.N)
	}
	if len(got.ByVendor) != 0 {
		t.Fatalf("ByVendor = %v, want empty for vendorless samples", got.ByVendor)
	}
}

func TestCompute_StageMedians(t *testing.T) {
	withStages := func(vendor, ordered, received string, stages map[string]float64) LeadTimeSample {
		s := sample(vendor, ordered, received)
		s.StageDurations = stages
		return s
	}
	samples := []LeadTimeSample{
		withStages("v1", "2026-07-01", "2026-07-11", map[string]float64{"sourcing": 2, "production": 5}),
		withStages("v1", "2026-07-02", "2026-07-14", map[string]float64{"sourcing": 4, "production": 7}),
		// Censored prefix (#29): this PO only recorded sourcing — its
		// absence from "production" must not count as a zero there.
		withStages("v1", "2026-07-03", "2026-07-15", map[string]float64{"sourcing": 6}),
		// "shipping" observed only once across all samples -> below
		// MinSamples, so no median may be reported for it.
		withStages("v1", "2026-07-04", "2026-07-16", map[string]float64{"sourcing": 8, "shipping": 3}),
	}
	got := Compute(samples).ByVendor["v1"]
	if got.N != 4 {
		t.Fatalf("N = %d, want 4", got.N)
	}
	// sourcing [2,4,6,8] -> median 5; production [5,7] -> median 6.
	if m := got.StageMedianDays["sourcing"]; !approxEqual(m, 5) {
		t.Fatalf("sourcing median = %v, want 5", m)
	}
	if m := got.StageMedianDays["production"]; !approxEqual(m, 6) {
		t.Fatalf("production median = %v, want 6", m)
	}
	if _, ok := got.StageMedianDays["shipping"]; ok {
		t.Fatal("shipping has one observation — below MinSamples, no median may be fabricated")
	}
}

// TestCompute_Deterministic pins the "same samples in any order" claim
// in Compute's doc comment.
func TestCompute_Deterministic(t *testing.T) {
	forward := []LeadTimeSample{
		sample("v1", "2026-07-01", "2026-07-11"),
		sample("v1", "2026-07-02", "2026-07-14"),
		sample("v2", "2026-07-03", "2026-07-10"),
		sample("v2", "2026-07-04", "2026-07-20"),
	}
	reversed := []LeadTimeSample{forward[3], forward[2], forward[1], forward[0]}

	a, b := Compute(forward), Compute(reversed)
	if a.Overall.N != b.Overall.N || a.Overall.P50Days != b.Overall.P50Days || a.Overall.P90Days != b.Overall.P90Days {
		t.Fatalf("overall differs by input order: %+v vs %+v", a.Overall, b.Overall)
	}
	for _, vendor := range []string{"v1", "v2"} {
		av, bv := a.ByVendor[vendor], b.ByVendor[vendor]
		if av.N != bv.N || av.P50Days != bv.P50Days || av.P90Days != bv.P90Days {
			t.Fatalf("%s differs by input order: %+v vs %+v", vendor, av, bv)
		}
	}
}
