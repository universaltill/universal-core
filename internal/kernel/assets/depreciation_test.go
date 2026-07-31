package assets

import (
	"math"
	"testing"
	"testing/quick"
	"time"
)

func TestBuild_StraightLineExact(t *testing.T) {
	got, err := Build(Input{
		Method:           MethodStraightLine,
		AcquisitionDate:  "2026-01-15",
		CostMinor:        120_000, // 1,200.00
		SalvageMinor:     0,
		UsefulLifeMonths: 12,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got) != 12 {
		t.Fatalf("expected 12 periods, got %d", len(got))
	}
	for i, p := range got {
		if p.DepreciationMinor != 10_000 {
			t.Errorf("period %d charge = %d, want 10000", i+1, p.DepreciationMinor)
		}
	}
	if got[0].PeriodEnd != "2026-01-31" {
		t.Errorf("first period ends %q, want 2026-01-31 (full-month convention)", got[0].PeriodEnd)
	}
	if got[1].PeriodEnd != "2026-02-28" {
		t.Errorf("second period ends %q, want 2026-02-28", got[1].PeriodEnd)
	}
	if last := got[11]; last.PeriodEnd != "2026-12-31" || last.BookValueMinor != 0 {
		t.Errorf("last period = %+v, want 2026-12-31 with a zero book value", last)
	}
}

// The remainder case: 1,000.00 over 3 months does not divide evenly.
// The charges must differ by at most one minor unit, the earliest
// periods carrying the extra, and the final book value must be exactly
// the salvage value — not "close to" it.
func TestBuild_RemainderDistributedExactly(t *testing.T) {
	got, err := Build(Input{
		Method:           MethodStraightLine,
		AcquisitionDate:  "2026-01-01",
		CostMinor:        100_000,
		SalvageMinor:     0,
		UsefulLifeMonths: 3,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []int64{33_334, 33_333, 33_333}
	var sum int64
	for i, p := range got {
		if p.DepreciationMinor != want[i] {
			t.Errorf("period %d charge = %d, want %d", i+1, p.DepreciationMinor, want[i])
		}
		sum += p.DepreciationMinor
	}
	if sum != 100_000 {
		t.Errorf("charges sum to %d, want exactly 100000", sum)
	}
	if got[2].BookValueMinor != 0 {
		t.Errorf("final book value = %d, want exactly 0", got[2].BookValueMinor)
	}
}

func TestBuild_SalvageValueRetained(t *testing.T) {
	got, err := Build(Input{
		Method:           MethodStraightLine,
		AcquisitionDate:  "2026-01-01",
		CostMinor:        500_000,
		SalvageMinor:     50_000,
		UsefulLifeMonths: 5,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got[4].BookValueMinor != 50_000 {
		t.Errorf("final book value = %d, want the salvage value 50000", got[4].BookValueMinor)
	}
	for _, p := range got {
		if p.DepreciationMinor != 90_000 {
			t.Errorf("charge = %d, want 90000 ((500000-50000)/5)", p.DepreciationMinor)
		}
	}
}

// A leap February and a 31st-of-the-month acquisition: the period ends
// must be real month-ends, and no month may be skipped (the trap in
// naive AddDate month arithmetic, where 2024-01-31 + 1 month lands in
// March).
func TestBuild_MonthEndsAndLeapYear(t *testing.T) {
	got, err := Build(Input{
		Method:           MethodStraightLine,
		AcquisitionDate:  "2024-01-31",
		CostMinor:        400,
		UsefulLifeMonths: 4,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []string{"2024-01-31", "2024-02-29", "2024-03-31", "2024-04-30"}
	for i, p := range got {
		if p.PeriodEnd != want[i] {
			t.Errorf("period %d ends %q, want %q", i+1, p.PeriodEnd, want[i])
		}
	}
}

func TestBuild_Rejections(t *testing.T) {
	base := Input{Method: MethodStraightLine, AcquisitionDate: "2026-01-01", CostMinor: 1000, UsefulLifeMonths: 12}
	for name, mutate := range map[string]func(*Input){
		"unsupported method": func(in *Input) { in.Method = "reducing_balance" },
		"bad date":           func(in *Input) { in.AcquisitionDate = "01/01/2026" },
		"zero life":          func(in *Input) { in.UsefulLifeMonths = 0 },
		"negative life":      func(in *Input) { in.UsefulLifeMonths = -12 },
		"negative cost":      func(in *Input) { in.CostMinor = -1 },
		"negative salvage":   func(in *Input) { in.SalvageMinor = -1 },
		"salvage over cost":  func(in *Input) { in.SalvageMinor = in.CostMinor + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			in := base
			mutate(&in)
			if got, err := Build(in); err == nil {
				t.Fatalf("Build accepted invalid input (%s): %+v", name, got)
			}
		})
	}
}

// TestBuild_SumsExactlyProperty is the invariant that matters more than
// any single example: for ANY valid cost/salvage/term, the charges sum
// to exactly cost-minus-salvage and the final book value is exactly the
// salvage value. This is the property a float implementation silently
// violates and an example-based test happens to miss.
func TestBuild_SumsExactlyProperty(t *testing.T) {
	f := func(cost, salvage uint32, months, year, month, day uint8) bool {
		c, s := int64(cost), int64(salvage)
		if s > c {
			c, s = s, c
		}
		m := int(months)%120 + 1 // 1..120 months
		// The acquisition date is generated too: the month walk is the
		// part most likely to regress, and pinning it to one start date
		// left it exercised only by the three hand-written examples
		// (independent review).
		acquired := time.Date(1970+int(year)%120, time.Month(int(month)%12+1), int(day)%28+1, 0, 0, 0, 0, time.UTC)
		periods, err := Build(Input{
			Method:           MethodStraightLine,
			AcquisitionDate:  acquired.Format("2006-01-02"),
			CostMinor:        c,
			SalvageMinor:     s,
			UsefulLifeMonths: m,
		})
		if err != nil || len(periods) != m {
			return false
		}
		var sum int64
		prev := int64(-1)
		prevEnd := ""
		for _, p := range periods {
			// Period ends are strictly increasing and distinct — a
			// skipped or repeated month is the classic month-arithmetic
			// bug and nothing else here would catch it.
			if p.PeriodEnd <= prevEnd {
				return false
			}
			prevEnd = p.PeriodEnd
			if p.DepreciationMinor < 0 {
				return false
			}
			// Charges are non-increasing (the earliest carry the
			// remainder) and never differ by more than one minor unit.
			if prev >= 0 && (p.DepreciationMinor > prev || prev-p.DepreciationMinor > 1) {
				return false
			}
			prev = p.DepreciationMinor
			sum += p.DepreciationMinor
		}
		return sum == c-s && periods[len(periods)-1].BookValueMinor == s
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatalf("depreciation invariant violated: %v", err)
	}
}

func TestMinorUnits(t *testing.T) {
	for _, tc := range []struct {
		v     float64
		scale int
		want  int64
	}{
		{1200, 2, 120_000},
		{1234.56, 2, 123_456},
		{0.005, 2, 1}, // representable above the half, so it rounds up
		{-0.005, 2, -1},
		{1000, 0, 1000}, // a zero-minor-unit currency
		{12.345, 3, 12_345},
		// The documented float artifact: 0.145 is stored as
		// 0.14499999…, so it rounds DOWN where decimal half-up would
		// give 15. Pinned so a future integer money type changes it
		// deliberately rather than by accident.
		{0.145, 2, 14},
	} {
		got, err := MinorUnits(tc.v, tc.scale)
		if err != nil {
			t.Errorf("MinorUnits(%v, %d): unexpected error %v", tc.v, tc.scale, err)
			continue
		}
		if got != tc.want {
			t.Errorf("MinorUnits(%v, %d) = %d, want %d", tc.v, tc.scale, got, tc.want)
		}
	}
}

// TestMinorUnits_Rejections is the regression test for the independent
// review's first blocker. Go makes an out-of-range float64→int64
// conversion IMPLEMENTATION-DEPENDENT: the reviewer compiled the old
// unchecked version for both architectures and got MaxInt64 on arm64
// where amd64 gave MinInt64 for the same input. This cluster is mixed
// arm64/amd64, so an asset with a huge cost produced a clean error on
// one node and a complete, fabricated depreciation schedule on another.
// Every one of these must now be an error on every architecture.
func TestMinorUnits_Rejections(t *testing.T) {
	for name, tc := range map[string]struct {
		v     float64
		scale int
	}{
		"NaN":                {math.NaN(), 2},
		"positive infinity":  {math.Inf(1), 2},
		"negative infinity":  {math.Inf(-1), 2},
		"overflows int64":    {1e18, 2},
		"overflows negative": {-1e18, 2},
		"overflows at 3dp":   {1e16, 3},
		"negative scale":     {100, -1},
		"absurd scale":       {100, 20},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := MinorUnits(tc.v, tc.scale); err == nil {
				t.Fatalf("MinorUnits(%v, %d) = %d, want an error", tc.v, tc.scale, got)
			}
		})
	}
	// The largest value that still converts cleanly must keep working —
	// the guard has to reject overflow, not sensible money.
	if _, err := MinorUnits(1e12, 2); err != nil {
		t.Errorf("a trillion at 2dp should convert fine: %v", err)
	}
}

// TestMinorUnits_NeverPanicsProperty: whatever float64 and scale arrive
// from a JSONB record, the converter either returns a usable value or
// an error — never a panic, and never a wrapped-around negative for a
// positive input (the arch-dependent failure mode).
func TestMinorUnits_NeverPanicsProperty(t *testing.T) {
	f := func(v float64, scale uint8) bool {
		got, err := MinorUnits(v, int(scale)%10)
		if err != nil {
			return true
		}
		if v > 0 && got < 0 {
			return false
		}
		if v < 0 && got > 0 {
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 5000}); err != nil {
		t.Fatalf("MinorUnits sign/panic invariant violated: %v", err)
	}
}

func TestBuild_RejectsAbsurdUsefulLife(t *testing.T) {
	if _, err := Build(Input{
		Method: MethodStraightLine, AcquisitionDate: "2026-01-01",
		CostMinor: 1000, UsefulLifeMonths: MaxUsefulLifeMonths + 1,
	}); err == nil {
		t.Fatal("a useful life beyond the cap must be rejected (an unbounded term allocates a Period per month)")
	}
	if _, err := Build(Input{
		Method: MethodStraightLine, AcquisitionDate: "2026-01-01",
		CostMinor: 1000, UsefulLifeMonths: MaxUsefulLifeMonths,
	}); err != nil {
		t.Fatalf("the cap itself must still be accepted: %v", err)
	}
}
