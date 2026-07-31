// Depreciation is valuation logic, so it follows the same discipline
// CLAUDE.md reserves for internal/kernel/ledger rather than the
// looser one a CRUD module gets: hand-written, exact, and property-
// tested — never "AI drafts it and the happy path passes."
//
// Two consequences visible in the code below:
//
//  1. **Integer minor units throughout.** A schedule computed in
//     float64 drifts, and the drift lands in the book value that a
//     balance sheet reports. Cost and salvage arrive as the float64
//     the entity engine stores for a FieldNumber, are converted to
//     minor units ONCE at the boundary, and every subsequent
//     operation is integer arithmetic.
//  2. **The periods sum exactly to the depreciable base.** Dividing
//     a base that isn't a whole multiple of the period count leaves a
//     remainder; spreading it by rounding each period independently
//     leaves the asset with a final book value that is not the
//     salvage value — off by a few minor units, every time, forever.
//     Build distributes the remainder across the earliest periods (one
//     extra minor unit each) so the sum is exact by construction, and
//     a property test asserts that for a large range of inputs rather
//     than for a handful of examples.
package assets

import (
	"fmt"
	"math"
	"time"
)

// Method names a depreciation method. Straight-line only for now —
// reducing-balance and units-of-production are real methods this
// kernel will need, but each changes the schedule shape and deserves
// its own tested implementation rather than a half-generalized one
// (the Definition's enum is the extension point).
type Method string

const (
	MethodStraightLine Method = "straight_line"
)

// Input is one asset's depreciation parameters, already read from its
// record.
type Input struct {
	// Method selects the schedule shape.
	Method Method
	// AcquisitionDate is when depreciation starts (ISO-8601 date). The
	// first period covers the month of acquisition — this kernel uses
	// full-month convention, not pro-rata by day, which is the common
	// default and the one a tenant can reason about without a policy
	// setting. Pro-rata is a per-jurisdiction refinement and therefore
	// plugin territory, not a core behaviour to guess at.
	AcquisitionDate string
	// CostMinor / SalvageMinor are integer minor units of the asset's
	// currency. Callers convert with MinorUnits so the float never
	// reaches the arithmetic.
	CostMinor    int64
	SalvageMinor int64
	// UsefulLifeMonths is the depreciation term. Expressed in months
	// rather than years so a schedule can be monthly without the
	// arithmetic having to re-derive it.
	UsefulLifeMonths int
}

// Period is one row of the resulting schedule.
type Period struct {
	// Sequence is 1-based, so a row reads the way an accountant counts.
	Sequence int
	// PeriodEnd is the ISO-8601 last day of the month this row covers —
	// the date a posting for it would carry.
	PeriodEnd string
	// DepreciationMinor is this period's charge; BookValueMinor is the
	// remaining carrying amount after it.
	DepreciationMinor int64
	BookValueMinor    int64
}

// MaxMinorUnitScale bounds the currency scale this converter accepts.
// No ISO 4217 currency has more than 4 decimal places; anything beyond
// that is a corrupt Currency record, not an exotic one.
const MaxMinorUnitScale = 6

// MaxUsefulLifeMonths caps a depreciation term at 100 years. Without a
// ceiling, a mistyped useful_life_months allocates a Period per month —
// 1e8 months is a multi-gigabyte allocation from one form field.
const MaxUsefulLifeMonths = 1200

// MinorUnits converts an entity FieldNumber value to integer minor
// units for the given currency scale, rounding to nearest (ties away
// from zero, as far as the float's own representation allows — see
// below).
//
// The float is the entity engine's storage type, not a choice made
// here; this function is the single boundary where it stops being one,
// which is exactly why it validates rather than trusting its input.
// FieldNumber accepts ANY float64, including NaN, ±Inf and values far
// beyond int64 — and Go makes an out-of-range float→int conversion
// *implementation-dependent*: the independent review compiled this for
// both architectures and got MaxInt64 on arm64 where amd64 gave
// MinInt64 for the same input. This product's own cluster is mixed
// arm64/amd64, so that is not a theoretical portability note: the same
// asset record produced a clean error on one node and a complete,
// fabricated depreciation schedule on another. An error return is the
// only honest answer.
//
// Rounding caveat: "ties away from zero" is exact only when the decimal
// is exactly representable. 0.145 is stored as 0.14499999…, so it
// converts to 14, not the 15 a person would write on paper. That is
// inherent to a float64 input and disappears when FieldNumber amounts
// move to an integer money type; it is not something this function can
// fix, so it is documented and pinned by a test rather than papered
// over.
func MinorUnits(v float64, minorUnit int) (int64, error) {
	if minorUnit < 0 || minorUnit > MaxMinorUnitScale {
		return 0, fmt.Errorf("assets: currency minor unit %d is out of range 0..%d", minorUnit, MaxMinorUnitScale)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("assets: amount %v is not a finite number", v)
	}
	scaled := math.Round(v * math.Pow(10, float64(minorUnit)))
	// The bound is exclusive because float64 cannot represent MaxInt64
	// exactly — the nearest double is 2^63, one above it — so comparing
	// against the float form of MaxInt64 would admit a value that
	// overflows on conversion.
	if math.Abs(scaled) >= math.Pow(2, 63) {
		return 0, fmt.Errorf("assets: amount %v exceeds what %d minor units can represent", v, minorUnit)
	}
	return int64(scaled), nil
}

// Build computes the full depreciation schedule. It validates its own
// inputs rather than trusting the record: a zero useful life, a
// salvage above cost, or an unparseable date each produce an error, so
// a bad asset record can never silently yield an empty or nonsensical
// schedule that then reads as "fully depreciated".
func Build(in Input) ([]Period, error) {
	if in.Method != MethodStraightLine {
		return nil, fmt.Errorf("assets: unsupported depreciation method %q", in.Method)
	}
	start, err := time.Parse("2006-01-02", in.AcquisitionDate)
	if err != nil {
		return nil, fmt.Errorf("assets: acquisition date %q is not an ISO-8601 date", in.AcquisitionDate)
	}
	if in.UsefulLifeMonths <= 0 || in.UsefulLifeMonths > MaxUsefulLifeMonths {
		return nil, fmt.Errorf("assets: useful life must be 1..%d months, got %d", MaxUsefulLifeMonths, in.UsefulLifeMonths)
	}
	if in.CostMinor < 0 || in.SalvageMinor < 0 {
		return nil, fmt.Errorf("assets: cost and salvage must not be negative (cost %d, salvage %d)", in.CostMinor, in.SalvageMinor)
	}
	if in.SalvageMinor > in.CostMinor {
		return nil, fmt.Errorf("assets: salvage value %d exceeds cost %d", in.SalvageMinor, in.CostMinor)
	}

	base := in.CostMinor - in.SalvageMinor
	n := int64(in.UsefulLifeMonths)
	per, remainder := base/n, base%n

	periods := make([]Period, 0, in.UsefulLifeMonths)
	book := in.CostMinor
	for i := 0; i < in.UsefulLifeMonths; i++ {
		amount := per
		// The remainder is spread one minor unit at a time across the
		// EARLIEST periods rather than dumped on the last one: the
		// difference is immaterial in total (it sums to the same base
		// either way) but it keeps every period's charge within one
		// minor unit of every other, which is what an auditor reading
		// the schedule expects to see.
		if int64(i) < remainder {
			amount++
		}
		book -= amount
		periods = append(periods, Period{
			Sequence:          i + 1,
			PeriodEnd:         endOfMonth(start, i),
			DepreciationMinor: amount,
			BookValueMinor:    book,
		})
	}
	return periods, nil
}

// endOfMonth returns the last day of the month i months after start,
// as an ISO-8601 date. Uses day 0 of the FOLLOWING month so month
// lengths and leap years are the standard library's problem, not this
// package's — and so an asset acquired on the 31st doesn't silently
// skip a month the way time.AddDate's day-overflow normalization would
// make it (2026-01-31 plus one month is 2026-03-03, not February).
func endOfMonth(start time.Time, i int) string {
	first := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	next := first.AddDate(0, i+1, 0)
	return next.AddDate(0, 0, -1).Format("2006-01-02")
}
