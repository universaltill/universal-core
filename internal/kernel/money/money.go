// Package money is this kernel's genuine "money via a money.Money-
// equivalent integer-minor-units type" (universal-core/CLAUDE.md's own
// stated API convention — until this package existed, nothing in the
// kernel actually implemented it: every price/total field was a plain
// entity.FieldNumber float64, and summing floats produces visible IEEE
// artifacts (0.1 + 0.2 = 0.30000000000000004) wherever more than one
// money value is added — found during independent review of #9 (RFQ
// vendor quote comparison), tracked as uc-infra#68).
//
// Money is a signed count of MINOR units (cents, fils, etc.) — never a
// float. Arithmetic on Money is exact int64 addition; there is no float
// anywhere in the hot path a caller sums prices through. The only place
// a float appears at all is at the two edges: parsing a human-typed
// decimal string in (ParseString) and formatting one back out
// (String/Major) — both single, deliberate, well-tested conversions, not
// a repeated accumulation.
//
// Scope of this package (first increment, uc-infra#68): Money itself is
// a currency-agnostic 2-decimal-place minor unit, matching this
// kernel's pre-existing simplification (foundation.Currency.minor_unit
// defaults to 2; internal/kernel/ledger.ToMinorUnits carries the
// identical assumption already). A real 0-decimal (JPY) or 3-decimal
// (KWD/BHD) currency needed a currency-aware decimal count — originally
// deferred "until a real currency-aware Definition needs it"
// (ADR-0021's own "Alternatives rejected"), a condition
// internal/kernel/assets hit first (depreciation valuation, its own
// currency_id-scoped Currency.minor_unit) and solved locally as
// assets.MinorUnits. uc-infra#163 promotes that proven logic here as
// FromMajorUnits, once a second, independent call site (sales/
// purchasing invoice posting) needed the identical conversion — see
// FromMajorUnits' own doc comment.
package money

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// MaxMinorUnitScale bounds the currency minor-unit scale FromMajorUnits
// accepts. No real-world ISO 4217 currency needs more than 4 decimal
// places (3 is already the practical maximum — KWD/BHD/OMR); this stays
// at 6 rather than tightening to 4 to leave headroom for a currency this
// kernel hasn't seen yet without another change here, while still
// bounding well short of where math.Pow(10, minorUnit) risks floating-
// point precision loss. A minor_unit outside [0, 6] is a corrupt
// Currency record, not an exotic one. Matches internal/kernel/assets'
// identical bound (assets.MaxMinorUnitScale is now an alias of this
// constant — see that package for why the name stayed for its own
// callers).
const MaxMinorUnitScale = 6

// Decimals is this package's current, documented minor-unit scale: 2
// decimal places for every amount, regardless of currency. See the
// package doc comment for why that's a deliberate, tracked
// simplification rather than an oversight.
const Decimals = 2

// scale is 10^Decimals — the conversion factor between a Money's minor
// units and its major-unit (float) representation.
const scale = 100

// Money is an exact count of minor units (e.g. US cents): Money(1050)
// is $10.50. Zero value is a genuine zero amount, not "unset" — callers
// distinguish "no value" the same way every other entity field does, at
// the map[string]any/presence layer, not inside this type.
type Money int64

// FromAny converts an already-decoded record value (what
// entity.ValidateRecord and every caller reading a stored record field
// sees: a JSON number decoded by encoding/json into float64, or a plain
// Go int/int64 from a caller that never round-tripped through JSON) into
// Money. It is the one gate that enforces "a money field's stored value
// is a whole number of minor units" — the actual bug-preventing check:
// a fractional value here (10.5 meaning "10.5 minor units", not a major-
// unit amount) means whatever produced it wasn't itself using minor
// units, and letting it through would silently reintroduce the float-
// scale ambiguity FieldMoney exists to remove.
func FromAny(v any) (Money, error) {
	switch n := v.(type) {
	case float64:
		if n != float64(int64(n)) {
			return 0, fmt.Errorf("money: %v is not a whole number of minor units", n)
		}
		return Money(int64(n)), nil
	case int:
		return Money(int64(n)), nil
	case int64:
		return Money(n), nil
	default:
		return 0, fmt.Errorf("money: expected a number, got %T", v)
	}
}

// FromMajorUnits converts a major-unit float64 amount (what a plain
// entity.FieldNumber price/total field stores) into an exact Money
// value at an explicit minor-unit scale — a validated replacement for
// hand-rolled `int64(math.Round(v * 100))`-style conversions wherever
// this package's own fixed Decimals=2 assumption isn't the right scale
// to convert AT (uc-infra#163).
//
// Passing a real per-record Currency.minor_unit here is NOT
// automatically safe just because the value is available — check what
// the CONVERTED result is stored into and read back by before doing
// that. uc-infra#163 originally wired this straight to CustomerInvoice/
// VendorInvoice's own currency_id, and independent review caught why
// that was wrong for both: the value each was about to feed
// (journal_lines.debit_minor/credit_minor, and a receivedMinor summed
// from money.Money-typed POLine.unit_price) has no per-value scale of
// its own recorded anywhere, and every other reader of it — internal/
// kernel/saft's formatMinor, Money.Major/String, a plain int64 sum on
// the other side of a comparison — unconditionally assumes this
// package's fixed Decimals. Converting at a real currency's own scale
// there would produce a technically-correct minor-unit count that
// every one of those readers then silently misreads by up to 100x. Only
// call this with a non-Decimals minorUnit when the value's eventual
// storage and every reader of it are ALL scale-aware together, not just
// the write path — internal/kernel/assets' depreciation postings are
// the one place in this kernel that's actually true today.
//

// The float is the entity engine's storage type, not a choice made
// here; this function is the boundary where it stops being one, which
// is exactly why it validates rather than trusting its input.
// FieldNumber accepts ANY float64, including NaN, ±Inf and values far
// beyond int64 — and Go makes an out-of-range float→int conversion
// *implementation-dependent*: originally found (as assets.MinorUnits,
// before this logic moved here) compiling identically for both
// architectures of this product's mixed arm64/amd64 cluster and getting
// MaxInt64 on one, MinInt64 on the other, for the same input. An error
// return is the only honest answer.
//
// Rounding caveat: "ties away from zero" is exact only when the decimal
// is exactly representable. 0.145 is stored as 0.14499999…, so it
// converts to 14, not the 15 a person would write on paper. That is
// inherent to a float64 input and disappears once a caller's amount
// starts as Money already; it is not something this function can fix,
// so it is documented and pinned by a test rather than papered over.
func FromMajorUnits(v float64, minorUnit int) (Money, error) {
	if minorUnit < 0 || minorUnit > MaxMinorUnitScale {
		return 0, fmt.Errorf("money: currency minor unit %d is out of range 0..%d", minorUnit, MaxMinorUnitScale)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("money: amount %v is not a finite number", v)
	}
	scaled := math.Round(v * math.Pow(10, float64(minorUnit)))
	// The bound is exclusive because float64 cannot represent MaxInt64
	// exactly — the nearest double is 2^63, one above it — so comparing
	// against the float form of MaxInt64 would admit a value that
	// overflows on conversion.
	if math.Abs(scaled) >= math.Pow(2, 63) {
		return 0, fmt.Errorf("money: amount %v exceeds what %d minor units can represent", v, minorUnit)
	}
	return Money(int64(scaled)), nil
}

// ParseString parses a human-typed major-unit decimal amount — "10.50",
// "10", "-3.4" — into Money. Canonical ASCII/dot-decimal input only, the
// same convention csvimport.Coerce's FieldNumber case already uses
// (strconv, not a locale-formatted string): round-tripping a form
// submission or CSV cell through regional grouping/decimal separators is
// a display-only concern (internal/kernel/locale's own scope-boundary
// doc comment), never something a stored value or its input widget
// renders. More than Decimals fractional digits is rejected rather than
// silently truncated — a typo'd extra digit must fail loud, not quietly
// drop a cent.
func ParseString(s string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("money: empty amount")
	}
	neg := false
	rest := s
	switch rest[0] {
	case '-':
		neg = true
		rest = rest[1:]
	case '+':
		rest = rest[1:]
	}
	whole, frac, hasFrac := strings.Cut(rest, ".")
	if whole == "" && (!hasFrac || frac == "") {
		return 0, fmt.Errorf("money: %q is not a valid amount", s)
	}
	// whole/frac must be plain digits — no embedded sign. Without this, a
	// doubled sign ("--5", "+-5") slips an extra '-' into whole/frac,
	// strconv.ParseInt happily parses THAT embedded sign too, and the
	// outer `if neg` flip below then double-negates it back to a wrong,
	// silently-accepted positive amount (independent review, uc-infra#68:
	// "--5" parsed as Money(500) instead of being rejected). Empty is
	// still fine for either half (".5" has an empty whole; "10" or a
	// bare trailing "10." has an empty frac) — only a NON-empty half
	// gets the digits-only check, so those existing, already-permitted
	// shapes don't regress.
	if whole != "" && !isDigits(whole) {
		return 0, fmt.Errorf("money: %q is not a valid amount", s)
	}
	if frac != "" && !isDigits(frac) {
		return 0, fmt.Errorf("money: %q is not a valid amount", s)
	}
	if hasFrac && len(frac) > Decimals {
		return 0, fmt.Errorf("money: %q has more than %d decimal places", s, Decimals)
	}
	for len(frac) < Decimals {
		frac += "0"
	}
	if whole == "" {
		whole = "0"
	}
	digits := whole + frac
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: %q is not a valid amount", s)
	}
	if neg {
		n = -n
	}
	return Money(n), nil
}

// isDigits reports whether s is non-empty and every byte is an ASCII
// digit — the guard ParseString uses to reject an embedded sign
// (strconv.ParseInt would otherwise happily accept one inside what's
// supposed to be a plain digit run). Deliberately ASCII-only: a
// human-typed amount is validated against the canonical, locale-
// independent decimal form this package's own doc comment describes,
// never a localized digit shape (Farsi/Arabic-Indic digits are a
// display-only concern, resolved by internal/kernel/locale, not here).
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Major returns the amount as a major-unit float — for handing to a
// locale-aware display formatter (internal/kernel/locale.Locale.
// FormatNumber) or any other genuinely display-only consumer. Never use
// this to accumulate multiple amounts; sum Money values first (plain
// int64 addition), convert to Major only at the final display step.
//
// Display-only precision ceiling: float64 stops representing every
// integer exactly above 2^53 (~9.007e15). At Decimals=2 that's an
// amount above roughly $90 trillion in minor units — display-only
// rounding at that scale, never affecting the underlying Money value
// (which stays exact int64 regardless), but worth knowing about before
// reusing this for anything beyond formatting.
func (m Money) Major() float64 {
	return float64(m) / scale
}

// String renders the canonical, locale-INDEPENDENT decimal form
// ("10.50", "-0.05") — what a form input's value attribute and a CSV
// export cell both use, so it round-trips back through ParseString
// unchanged (the same "stored/canonical value in the input, locale
// formatting only for read-only display" discipline
// internal/kernel/locale's own doc comment establishes for dates).
func (m Money) String() string {
	// Built from strconv.FormatInt's own digit string, not by negating
	// int64(m) directly: negating math.MinInt64 overflows back to
	// itself (two's-complement has no positive counterpart for the most
	// negative value), which would have produced garbage output for
	// that one, admittedly extreme, amount — independent review,
	// uc-infra#68. FormatInt has no such edge case; it already handles
	// the full int64 range correctly.
	s := strconv.FormatInt(int64(m), 10)
	neg := strings.HasPrefix(s, "-")
	digits := strings.TrimPrefix(s, "-")
	for len(digits) < Decimals {
		digits = "0" + digits
	}
	whole, frac := digits[:len(digits)-Decimals], digits[len(digits)-Decimals:]
	if whole == "" {
		whole = "0"
	}
	sign := ""
	if neg {
		sign = "-"
	}
	return sign + whole + "." + frac
}

// Sum adds a slice of Money values with plain, exact int64 addition —
// no float ever enters this computation. Exists mostly for callers that
// have a slice in hand already (a native loop with `+=` works identically
// and needs no helper); its real purpose is being the one place a test
// pins the exact scenario this package was built to fix.
func Sum(ms ...Money) Money {
	var total Money
	for _, m := range ms {
		total += m
	}
	return total
}
