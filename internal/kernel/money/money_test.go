package money

import (
	"math"
	"testing"
	"testing/quick"
)

// TestSum_NoFloatArtifact pins the exact bug this package exists to fix
// (uc-infra#68): summing amounts that produce a visible IEEE artifact in
// float64 major-unit arithmetic (0.1 + 0.2 == 0.30000000000000004, not
// 0.3) must produce an exact result in minor-unit Money arithmetic.
func TestSum_NoFloatArtifact(t *testing.T) {
	// Runtime float64 variables, not untyped constants: Go's compiler
	// evaluates a constant expression like "0.1 + 0.2" with arbitrary
	// precision and rounds only once, which would mask the exact bug
	// this test exists to pin — the artifact only shows up when the
	// addition itself happens in float64 at runtime.
	var fa, fb float64 = 0.1, 0.2
	if got := fa + fb; got == 0.3 {
		t.Fatalf("test premise broken: float64 0.1+0.2 no longer reproduces the artifact (got %v)", got)
	}
	a, err := ParseString("0.10")
	if err != nil {
		t.Fatalf("ParseString(0.10): %v", err)
	}
	b, err := ParseString("0.20")
	if err != nil {
		t.Fatalf("ParseString(0.20): %v", err)
	}
	total := Sum(a, b)
	if total != 30 {
		t.Fatalf("Sum(0.10, 0.20) = %d minor units, want 30", int64(total))
	}
	if got := total.String(); got != "0.30" {
		t.Fatalf("total.String() = %q, want %q", got, "0.30")
	}
}

func TestParseString(t *testing.T) {
	cases := []struct {
		in      string
		want    Money
		wantErr bool
	}{
		{in: "10.50", want: 1050},
		{in: "10", want: 1000},
		{in: "0", want: 0},
		{in: "-3.40", want: -340},
		{in: "+3.40", want: 340},
		{in: ".5", want: 50},
		{in: "0.05", want: 5},
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "10.505", wantErr: true}, // more than 2 decimal places
		{in: "-", wantErr: true},
		{in: ".", wantErr: true},
		// Regression cases (independent review, uc-infra#68): a doubled
		// sign must not slip through ParseInt parsing the embedded sign
		// itself, which used to silently produce a wrong-signed result
		// ("--5" -> Money(500) instead of an error).
		{in: "--5", wantErr: true},
		{in: "+-5", wantErr: true},
		{in: "-+5", wantErr: true},
		{in: "++5", wantErr: true},
		{in: "1-2", wantErr: true},
		{in: "1.-5", wantErr: true},
		{in: "1.2-", wantErr: true},
		// Overflow: every character is a plain digit (passes the
		// isDigits guard), but the combined minor-units integer exceeds
		// int64 range — strconv.ParseInt's own overflow error, not the
		// digits-shape check above.
		{in: "99999999999999999999", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseString(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseString(%q): expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseString(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseString(%q) = %d, want %d", c.in, int64(got), int64(c.want))
		}
	}
}

func TestMoney_String(t *testing.T) {
	cases := []struct {
		in   Money
		want string
	}{
		{in: 1050, want: "10.50"},
		{in: 0, want: "0.00"},
		{in: -340, want: "-3.40"},
		{in: 5, want: "0.05"},
		{in: -5, want: "-0.05"},
		{in: 100, want: "1.00"},
		// math.MinInt64 (independent review, uc-infra#68): negating
		// int64(m) directly overflows back to itself for this one
		// value (two's-complement has no positive counterpart), which
		// used to produce garbage output. String() is now built from
		// strconv.FormatInt's own digit string instead, which has no
		// such edge case.
		{in: math.MinInt64, want: "-92233720368547758.08"},
		{in: math.MaxInt64, want: "92233720368547758.07"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("Money(%d).String() = %q, want %q", int64(c.in), got, c.want)
		}
	}
}

// TestParseString_RoundTrip: every value String() produces must parse
// back to the identical Money — the exact round-trip ParseString's own
// doc comment promises for a form input's value attribute / CSV export
// cell.
func TestParseString_RoundTrip(t *testing.T) {
	for _, m := range []Money{0, 1050, -1050, 5, -5, 999999999} {
		s := m.String()
		got, err := ParseString(s)
		if err != nil {
			t.Fatalf("ParseString(%q) (round-trip of %d): %v", s, int64(m), err)
		}
		if got != m {
			t.Fatalf("round-trip: Money(%d).String() = %q, ParseString back = %d", int64(m), s, int64(got))
		}
	}
}

func TestFromAny(t *testing.T) {
	cases := []struct {
		in      any
		want    Money
		wantErr bool
	}{
		{in: float64(1050), want: 1050},
		{in: float64(0), want: 0},
		{in: float64(-340), want: -340},
		{in: 1050, want: 1050},        // int
		{in: int64(1050), want: 1050}, // int64
		{in: float64(10.5), wantErr: true},
		{in: "1050", wantErr: true},
		{in: nil, wantErr: true},
		{in: true, wantErr: true},
	}
	for _, c := range cases {
		got, err := FromAny(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("FromAny(%v): expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("FromAny(%v): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("FromAny(%v) = %d, want %d", c.in, int64(got), int64(c.want))
		}
	}
}

func TestMoney_Major(t *testing.T) {
	if got := Money(1050).Major(); got != 10.5 {
		t.Errorf("Money(1050).Major() = %v, want 10.5", got)
	}
	if got := Money(-50).Major(); got != -0.5 {
		t.Errorf("Money(-50).Major() = %v, want -0.5", got)
	}
}

// TestIsDigits exercises the helper directly — ParseString's own call
// sites only ever pass it a non-empty whole/frac (guarded by `whole !=
// "" && ...`), so the empty-string branch needs its own direct test to
// reach 100% coverage.
func TestIsDigits(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{in: "", want: false},
		{in: "0", want: true},
		{in: "12345", want: true},
		{in: "-5", want: false},
		{in: "+5", want: false},
		{in: "1.5", want: false},
		{in: "1a", want: false},
	}
	for _, c := range cases {
		if got := isDigits(c.in); got != c.want {
			t.Errorf("isDigits(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSum_Empty(t *testing.T) {
	if got := Sum(); got != 0 {
		t.Errorf("Sum() = %d, want 0", int64(got))
	}
}

// TestFromMajorUnits is the canonical currency-aware conversion table
// test (uc-infra#163) — promoted verbatim in spirit from
// internal/kernel/assets' own TestMinorUnits, which exercised the
// identical logic before it moved here.
func TestFromMajorUnits(t *testing.T) {
	for _, tc := range []struct {
		v         float64
		minorUnit int
		want      int64
	}{
		{1200, 2, 120_000},
		{1234.56, 2, 123_456},
		{0.005, 2, 1}, // representable above the half, so it rounds up
		{-0.005, 2, -1},
		{1000, 0, 1000},     // a zero-decimal currency (JPY-style)
		{12.345, 3, 12_345}, // a three-decimal currency (KWD/BHD-style)
		// The documented float artifact: 0.145 is stored as
		// 0.14499999…, so it rounds DOWN where decimal half-up would
		// give 15. Pinned so a future change to this rounding is
		// deliberate, not accidental.
		{0.145, 2, 14},
	} {
		got, err := FromMajorUnits(tc.v, tc.minorUnit)
		if err != nil {
			t.Errorf("FromMajorUnits(%v, %d): unexpected error %v", tc.v, tc.minorUnit, err)
			continue
		}
		if int64(got) != tc.want {
			t.Errorf("FromMajorUnits(%v, %d) = %d, want %d", tc.v, tc.minorUnit, int64(got), tc.want)
		}
	}
}

// TestFromMajorUnits_Rejections mirrors assets.MinorUnits' own
// regression test for the independent review's arch-dependent
// float64->int64 conversion finding: this cluster is mixed arm64/amd64,
// and an unchecked conversion produced a clean error on one node and a
// fabricated amount on the other for the same input. Every one of these
// must now be an error on every architecture.
func TestFromMajorUnits_Rejections(t *testing.T) {
	for name, tc := range map[string]struct {
		v         float64
		minorUnit int
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
			if got, err := FromMajorUnits(tc.v, tc.minorUnit); err == nil {
				t.Fatalf("FromMajorUnits(%v, %d) = %d, want an error", tc.v, tc.minorUnit, int64(got))
			}
		})
	}
	// The largest value that still converts cleanly must keep working —
	// the guard has to reject overflow, not sensible money.
	if _, err := FromMajorUnits(1e12, 2); err != nil {
		t.Errorf("a trillion at 2dp should convert fine: %v", err)
	}
}

// TestFromMajorUnits_NeverPanicsProperty: whatever float64 and scale
// arrive from a JSONB record, the converter either returns a usable
// value or an error — never a panic, and never a wrapped-around
// negative for a positive input (the arch-dependent failure mode).
func TestFromMajorUnits_NeverPanicsProperty(t *testing.T) {
	f := func(v float64, scale uint8) bool {
		got, err := FromMajorUnits(v, int(scale)%10)
		if err != nil {
			return true
		}
		if v > 0 && int64(got) < 0 {
			return false
		}
		if v < 0 && int64(got) > 0 {
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 5000}); err != nil {
		t.Fatalf("FromMajorUnits sign/panic invariant violated: %v", err)
	}
}
