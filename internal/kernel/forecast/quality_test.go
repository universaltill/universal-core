package forecast

import "testing"

func qualitySample(vendor string, accepted, rejected float64) QualitySample {
	return QualitySample{VendorID: vendor, QtyAccepted: accepted, QtyRejected: rejected, HasData: true}
}

func TestComputeQuality_OverallAndPerVendor(t *testing.T) {
	// Vendor A: two lines, 18 accepted / 2 rejected across both ->
	// 18/20 = 90%. Vendor B: one line only, insufficient.
	samples := []QualitySample{
		qualitySample("vendor-a", 8, 2),
		qualitySample("vendor-a", 10, 0),
		qualitySample("vendor-b", 5, 5), // N=1, insufficient
	}
	got := ComputeQuality(samples)

	if got.Overall.N != 3 {
		t.Fatalf("Overall.N = %d, want 3", got.Overall.N)
	}
	if !got.Overall.Sufficient() {
		t.Fatal("Overall with 3 samples must be sufficient")
	}
	// Overall: 23 accepted (8+10+5) / 30 total (8+2+10+0+5+5) -> 23/30.
	if !approxEqual(got.Overall.Rate(), 23.0/30.0) {
		t.Fatalf("Overall.Rate() = %v, want %v", got.Overall.Rate(), 23.0/30.0)
	}

	a := got.ByVendor["vendor-a"]
	if a.N != 2 || !a.Sufficient() {
		t.Fatalf("vendor-a = %+v, want N=2 sufficient", a)
	}
	if !approxEqual(a.Rate(), 18.0/20.0) {
		t.Fatalf("vendor-a.Rate() = %v, want %v", a.Rate(), 18.0/20.0)
	}

	b := got.ByVendor["vendor-b"]
	if b.N != 1 {
		t.Fatalf("vendor-b.N = %d, want 1", b.N)
	}
	if b.Sufficient() {
		t.Fatal("vendor-b with one sample must be insufficient — never a fabricated rate")
	}
}

func TestComputeQuality_QuantityWeightedNotSampleAveraged(t *testing.T) {
	// One large defective shipment (10 accepted / 90 rejected) must not
	// be diluted to the same weight as a single-unit perfect line (1
	// accepted / 0 rejected) — a naive average-of-per-line-rates would
	// report (100% + 10%) / 2 = 55%; the quantity-weighted rate must
	// report 11/101 instead.
	samples := []QualitySample{
		qualitySample("vendor-a", 1, 0),
		qualitySample("vendor-a", 10, 90),
	}
	got := ComputeQuality(samples)
	a := got.ByVendor["vendor-a"]
	want := 11.0 / 101.0
	if !approxEqual(a.Rate(), want) {
		t.Fatalf("Rate() = %v, want quantity-weighted %v (not the naive average 0.55)", a.Rate(), want)
	}
}

func TestComputeQuality_EmptyAndSingleSample(t *testing.T) {
	empty := ComputeQuality(nil)
	if empty.Overall.N != 0 || empty.Overall.Sufficient() {
		t.Fatalf("empty input: Overall = %+v, want N=0 insufficient", empty.Overall)
	}
	if len(empty.ByVendor) != 0 {
		t.Fatalf("empty input: ByVendor = %v, want empty", empty.ByVendor)
	}
	if empty.Overall.Rate() != 0 {
		t.Fatalf("empty input: Rate() = %v, want 0 (divide-by-zero guarded)", empty.Overall.Rate())
	}

	one := ComputeQuality([]QualitySample{qualitySample("v1", 5, 0)})
	if one.Overall.N != 1 || one.Overall.Sufficient() {
		t.Fatalf("single sample: Overall = %+v, want N=1 insufficient", one.Overall)
	}
}

// TestComputeQuality_SkipsSamplesWithNoData: most GoodsReceiptLines have
// neither qty_accepted nor qty_rejected set (uc-infra#82: optional
// fields) — those must be excluded from both N and the rate, not
// counted as a 0% or 100% outcome.
func TestComputeQuality_SkipsSamplesWithNoData(t *testing.T) {
	samples := []QualitySample{
		qualitySample("v1", 5, 0),
		{VendorID: "v1", QtyAccepted: 0, QtyRejected: 0, HasData: false}, // no quality data — skipped
	}
	got := ComputeQuality(samples)
	v := got.ByVendor["v1"]
	if v.N != 1 {
		t.Fatalf("N = %d, want 1 (a line with no quality data must not count)", v.N)
	}
}

// TestComputeQuality_ZeroAcceptedZeroRejectedWithDataStillCounts pins
// the distinction QualitySample.HasData exists for: an explicit 0/0
// quality record (a real, if unusual, outcome — nothing was inspected
// as good or bad, e.g. a zero-qty correction line) is NOT the same as
// "no quality data" and must still count toward N (even though it
// contributes nothing to either total).
func TestComputeQuality_ZeroAcceptedZeroRejectedWithDataStillCounts(t *testing.T) {
	samples := []QualitySample{{VendorID: "v1", QtyAccepted: 0, QtyRejected: 0, HasData: true}}
	got := ComputeQuality(samples)
	v := got.ByVendor["v1"]
	if v.N != 1 {
		t.Fatalf("N = %d, want 1 (HasData=true must count even at 0/0)", v.N)
	}
}

// TestComputeQuality_NegativeQuantitiesExcluded is a defensive check:
// purchasing.validateGoodsReceiptLineQuality already rejects negative
// quantities at write time, but this package must not silently corrupt
// its own aggregate if a negative value reaches it some other way (a
// pre-hook CSV import, a future write path).
func TestComputeQuality_NegativeQuantitiesExcluded(t *testing.T) {
	samples := []QualitySample{
		{VendorID: "v1", QtyAccepted: -1, QtyRejected: 5, HasData: true},
		{VendorID: "v1", QtyAccepted: 5, QtyRejected: -1, HasData: true},
		qualitySample("v1", 5, 5),
	}
	got := ComputeQuality(samples)
	v := got.ByVendor["v1"]
	if v.N != 1 {
		t.Fatalf("N = %d, want 1 (both negative-quantity samples must be excluded)", v.N)
	}
}

// TestComputeQuality_VendorlessSamplesCountOnlyOverall mirrors
// ComputeOnTime's own equivalent behavior.
func TestComputeQuality_VendorlessSamplesCountOnlyOverall(t *testing.T) {
	samples := []QualitySample{
		qualitySample("", 5, 0),
		qualitySample("", 3, 1),
	}
	got := ComputeQuality(samples)
	if got.Overall.N != 2 {
		t.Fatalf("Overall.N = %d, want 2", got.Overall.N)
	}
	if len(got.ByVendor) != 0 {
		t.Fatalf("ByVendor = %v, want empty for vendorless samples", got.ByVendor)
	}
}

// TestComputeQuality_Deterministic pins the "same samples in any order"
// claim in ComputeQuality's doc comment.
func TestComputeQuality_Deterministic(t *testing.T) {
	forward := []QualitySample{
		qualitySample("v1", 8, 2),
		qualitySample("v1", 4, 1),
		qualitySample("v2", 9, 0),
		qualitySample("v2", 2, 3),
	}
	reversed := []QualitySample{forward[3], forward[2], forward[1], forward[0]}

	a, b := ComputeQuality(forward), ComputeQuality(reversed)
	if a.Overall.N != b.Overall.N || !approxEqual(a.Overall.Rate(), b.Overall.Rate()) {
		t.Fatalf("overall differs by input order: %+v vs %+v", a.Overall, b.Overall)
	}
	for _, vendor := range []string{"v1", "v2"} {
		av, bv := a.ByVendor[vendor], b.ByVendor[vendor]
		if av.N != bv.N || !approxEqual(av.Rate(), bv.Rate()) {
			t.Fatalf("%s differs by input order: %+v vs %+v", vendor, av, bv)
		}
	}
}
