package forecast

import "testing"

func onTimeSample(vendor, promised, received string) OnTimeSample {
	return OnTimeSample{VendorID: vendor, PromisedDate: day(promised), ReceivedDate: day(received)}
}

func TestOnTimeSample_OnTime(t *testing.T) {
	tests := []struct {
		name       string
		promised   string
		received   string
		wantOnTime bool
	}{
		{"received before promise is on time", "2026-07-20", "2026-07-18", true},
		{"received exactly on promise is on time", "2026-07-20", "2026-07-20", true},
		{"received after promise is late", "2026-07-20", "2026-07-21", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := onTimeSample("v1", tc.promised, tc.received)
			if got := s.OnTime(); got != tc.wantOnTime {
				t.Fatalf("OnTime() = %v, want %v", got, tc.wantOnTime)
			}
		})
	}
}

func TestComputeOnTime_OverallAndPerVendor(t *testing.T) {
	// Vendor A: 2 on-time, 1 late -> rate 2/3. Vendor B: 1 sample only,
	// insufficient. Overall: 3 on-time, 1 late across 4 samples -> 3/4.
	samples := []OnTimeSample{
		onTimeSample("vendor-a", "2026-07-20", "2026-07-18"), // on time
		onTimeSample("vendor-a", "2026-07-25", "2026-07-25"), // on time (boundary)
		onTimeSample("vendor-a", "2026-07-10", "2026-07-14"), // late
		onTimeSample("vendor-b", "2026-06-01", "2026-05-30"), // on time, N=1
	}
	got := ComputeOnTime(samples)

	if got.Overall.N != 4 {
		t.Fatalf("Overall.N = %d, want 4", got.Overall.N)
	}
	if got.Overall.OnTimeN != 3 {
		t.Fatalf("Overall.OnTimeN = %d, want 3", got.Overall.OnTimeN)
	}
	if !got.Overall.Sufficient() {
		t.Fatal("Overall with 4 samples must be sufficient")
	}
	if !approxEqual(got.Overall.Rate(), 0.75) {
		t.Fatalf("Overall.Rate() = %v, want 0.75", got.Overall.Rate())
	}

	a := got.ByVendor["vendor-a"]
	if a.N != 3 || a.OnTimeN != 2 || !a.Sufficient() {
		t.Fatalf("vendor-a = %+v, want N=3 OnTimeN=2 sufficient", a)
	}
	if !approxEqual(a.Rate(), 2.0/3.0) {
		t.Fatalf("vendor-a.Rate() = %v, want %v", a.Rate(), 2.0/3.0)
	}

	b := got.ByVendor["vendor-b"]
	if b.N != 1 {
		t.Fatalf("vendor-b.N = %d, want 1", b.N)
	}
	if b.Sufficient() {
		t.Fatal("vendor-b with one sample must be insufficient — never a fabricated rate")
	}
}

func TestComputeOnTime_EmptyAndSingleSample(t *testing.T) {
	empty := ComputeOnTime(nil)
	if empty.Overall.N != 0 || empty.Overall.Sufficient() {
		t.Fatalf("empty input: Overall = %+v, want N=0 insufficient", empty.Overall)
	}
	if len(empty.ByVendor) != 0 {
		t.Fatalf("empty input: ByVendor = %v, want empty", empty.ByVendor)
	}
	if empty.Overall.Rate() != 0 {
		t.Fatalf("empty input: Rate() = %v, want 0 (divide-by-zero guarded)", empty.Overall.Rate())
	}

	one := ComputeOnTime([]OnTimeSample{onTimeSample("v1", "2026-07-01", "2026-07-01")})
	if one.Overall.N != 1 || one.Overall.Sufficient() {
		t.Fatalf("single sample: Overall = %+v, want N=1 insufficient", one.Overall)
	}
}

// TestComputeOnTime_SkipsSamplesMissingEitherDate: most completed POs
// have no promised_delivery_date at all (#11: optional field) — those
// rows must be excluded from both N and the rate, not counted as a
// missed promise.
func TestComputeOnTime_SkipsSamplesMissingEitherDate(t *testing.T) {
	samples := []OnTimeSample{
		onTimeSample("v1", "2026-07-01", "2026-07-01"),
		{VendorID: "v1", ReceivedDate: day("2026-07-10")}, // no promise — skipped
		{VendorID: "v1", PromisedDate: day("2026-07-10")}, // no receipt — skipped
	}
	got := ComputeOnTime(samples)
	v := got.ByVendor["v1"]
	if v.N != 1 {
		t.Fatalf("N = %d, want 1 (samples missing either date must not count)", v.N)
	}
}

// TestComputeOnTime_LateDeliveryNotDroppedAsInvalid: a promise date that
// falls AFTER the receipt date (an early delivery) is a valid, on-time
// sample — must not be mistaken for LeadTimeSample's "reversed noise"
// exclusion rule, which does not apply to this metric.
func TestComputeOnTime_EarlyDeliveryCounts(t *testing.T) {
	samples := []OnTimeSample{
		onTimeSample("v1", "2026-07-20", "2026-07-10"), // promised AFTER actual receipt: early, on time
	}
	got := ComputeOnTime(samples)
	v := got.ByVendor["v1"]
	if v.N != 1 || v.OnTimeN != 1 {
		t.Fatalf("early delivery = %+v, want N=1 OnTimeN=1", v)
	}
}

// TestComputeOnTime_VendorlessSamplesCountOnlyOverall mirrors Compute's
// own equivalent behavior for lead-time samples.
func TestComputeOnTime_VendorlessSamplesCountOnlyOverall(t *testing.T) {
	samples := []OnTimeSample{
		onTimeSample("", "2026-07-01", "2026-07-01"),
		onTimeSample("", "2026-07-02", "2026-07-05"),
	}
	got := ComputeOnTime(samples)
	if got.Overall.N != 2 {
		t.Fatalf("Overall.N = %d, want 2", got.Overall.N)
	}
	if len(got.ByVendor) != 0 {
		t.Fatalf("ByVendor = %v, want empty for vendorless samples", got.ByVendor)
	}
}

// TestComputeOnTime_Deterministic pins the "same samples in any order"
// claim in ComputeOnTime's doc comment.
func TestComputeOnTime_Deterministic(t *testing.T) {
	forward := []OnTimeSample{
		onTimeSample("v1", "2026-07-01", "2026-07-01"),
		onTimeSample("v1", "2026-07-02", "2026-07-05"),
		onTimeSample("v2", "2026-07-03", "2026-07-01"),
		onTimeSample("v2", "2026-07-04", "2026-07-20"),
	}
	reversed := []OnTimeSample{forward[3], forward[2], forward[1], forward[0]}

	a, b := ComputeOnTime(forward), ComputeOnTime(reversed)
	if a.Overall.N != b.Overall.N || a.Overall.OnTimeN != b.Overall.OnTimeN {
		t.Fatalf("overall differs by input order: %+v vs %+v", a.Overall, b.Overall)
	}
	for _, vendor := range []string{"v1", "v2"} {
		av, bv := a.ByVendor[vendor], b.ByVendor[vendor]
		if av.N != bv.N || av.OnTimeN != bv.OnTimeN {
			t.Fatalf("%s differs by input order: %+v vs %+v", vendor, av, bv)
		}
	}
}
