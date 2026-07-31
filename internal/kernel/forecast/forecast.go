// Package forecast is the classical-statistics lead-time service behind
// universal-core#30 — BACKLOG.md R10's "predictive = classical stats
// kernel service" worked example. PURE functions only: no SQL, no HTTP,
// no clock — callers (internal/api/reporting.go) fetch completed
// purchase orders through internal/data and hand fully-resolved samples
// in, which is what keeps every quantile deterministic and unit-testable
// without a database.
//
// Deliberately scoped to this single consumer (the purchasing report's
// P50/P90 supplier lead times + reorder signals). Generalizing it into a
// cross-module prediction service is R10's later arc and gets its own
// ADR when a SECOND consumer actually exists — doing it now, against one
// example, would be exactly the premature abstraction
// universal-core/CLAUDE.md warns about (recorded in issue #30's
// Architect design comment; same reasoning as PurchaseOrder's own
// one-off-backfill note in internal/kernel/purchasing).
//
// Minimum-sample rule: with fewer than MinSamples observations no
// quantile is ever reported — an "insufficient history" outcome is an
// explicit, checkable state (LeadTimeStats.Sufficient), never a
// fabricated number extrapolated from one data point.
package forecast

import (
	"math"
	"sort"
	"time"
)

// MinSamples is the minimum number of observations required before any
// quantile is reported. Below this, LeadTimeStats.Sufficient() is false
// and P50Days/P90Days are meaningless zeroes callers must not render —
// one observation is an anecdote, not a distribution.
const MinSamples = 2

// Quantile returns the exact empirical q-quantile (0 <= q <= 1) of
// samples with linear interpolation between order statistics — the
// standard type-7 estimator (R's default): for n sorted values, the
// quantile sits at rank h = (n-1)*q, interpolating linearly between the
// values at floor(h) and floor(h)+1. Exact and deterministic; no
// distributional assumption — appropriate here because n (completed POs
// per vendor) is small, per issue #30's BA note.
//
// Input need NOT be sorted: Quantile sorts a private copy internally
// (deliberate deviation from taking pre-sorted input — a caller passing
// unsorted data would otherwise get a silently wrong number back, the
// worst failure mode for a page that recommends spending money; at the
// n this service sees, copying+sorting is free). The caller's slice is
// never mutated.
//
// ok is false when samples is empty (there is no quantile of nothing) or
// q is outside [0, 1] — including NaN, which every comparison lets
// through (NaN < 0 and NaN > 1 are both false) and which would
// otherwise reach int(math.Floor(NaN)), a platform-divergent conversion
// (undefined per the Go spec; panics on some architectures). ±Inf are
// already caught by the range checks. A single sample IS its own
// quantile for every q — enforcing MinSamples is LeadTimeStats' job (a
// policy about when a statistic is trustworthy), not Quantile's (pure
// math).
func Quantile(samples []float64, q float64) (value float64, ok bool) {
	if len(samples) == 0 || math.IsNaN(q) || q < 0 || q > 1 {
		return 0, false
	}
	sorted := make([]float64, len(samples))
	copy(sorted, samples)
	sort.Float64s(sorted)

	h := float64(len(sorted)-1) * q
	lo := int(math.Floor(h))
	if lo >= len(sorted)-1 {
		return sorted[len(sorted)-1], true
	}
	return sorted[lo] + (h-float64(lo))*(sorted[lo+1]-sorted[lo]), true
}

// Fires is the reorder-signal predicate (universal-core#30's BA R3,
// hand-reviewed in the issue's design comment — it recommends spending
// money): a rule fires when the inventory position (qty on hand + open
// on-order qty) is at or below reorder_point + safety_stock. The
// boundary is deliberately inclusive: at exactly the threshold the
// buyer is already out of buffer, so waiting for one more unit of drift
// would fire the signal strictly late. A missing safety_stock is the
// caller's zero.
func Fires(position, reorderPoint, safetyStock float64) bool {
	return position <= reorderPoint+safetyStock
}

// LeadTimeSample is one completed purchase order's observed lead time:
// order placed at OrderDate, goods received at ReceivedDate (either the
// PO's own received_at stage or the query-time GoodsReceipt fallback —
// resolved by the caller, this package doesn't care which).
// StageDurations optionally carries per-stage durations in days
// (sourcing, production, shipping, …) for stage-median aggregation —
// keyed by stage name, only for the stages this PO actually recorded
// (#29's censored-prefix reality).
type LeadTimeSample struct {
	VendorID       string
	OrderDate      time.Time
	ReceivedDate   time.Time
	StageDurations map[string]float64
}

// LeadTimeDays is this sample's total lead time in fractional days.
func (s LeadTimeSample) LeadTimeDays() float64 {
	return s.ReceivedDate.Sub(s.OrderDate).Hours() / 24
}

// valid reports whether this sample is usable: both endpoints present
// and received not before ordered. Stage dates are noisy user input
// (issue #29's review: chronology validation is data hygiene, not
// ledger-grade, and imported dates can bypass it entirely), so a
// reversed or half-empty sample is dropped from the aggregation rather
// than poisoning every quantile with a negative or nonsense duration.
func (s LeadTimeSample) valid() bool {
	return !s.OrderDate.IsZero() && !s.ReceivedDate.IsZero() && !s.ReceivedDate.Before(s.OrderDate)
}

// LeadTimeStats is the aggregate over a set of samples. P50Days/P90Days
// are only meaningful when Sufficient() — callers must render an
// insufficient-history state otherwise, never the zeroes.
// StageMedianDays holds the median duration per stage name, computed
// over the samples that recorded that stage, and only for stages with
// at least MinSamples observations (the same no-fabrication rule,
// applied per stage).
type LeadTimeStats struct {
	N               int
	P50Days         float64
	P90Days         float64
	StageMedianDays map[string]float64
}

// Sufficient reports whether enough history exists for the quantiles to
// be trustworthy (N >= MinSamples).
func (s LeadTimeStats) Sufficient() bool {
	return s.N >= MinSamples
}

// Result is Compute's output: overall stats across every vendor, plus a
// per-vendor breakdown keyed by VendorID. A vendor appears in ByVendor
// with whatever N it has — including an insufficient N of 1, so the
// report can still show "this vendor has 1 completed order" honestly.
type Result struct {
	Overall  LeadTimeStats
	ByVendor map[string]LeadTimeStats
}

// Compute aggregates samples into overall + per-vendor lead-time stats.
// Invalid samples (missing endpoints, received before ordered — see
// LeadTimeSample.valid) are skipped entirely; they count toward nothing.
// Deterministic: same samples in any order produce identical output.
func Compute(samples []LeadTimeSample) Result {
	byVendor := map[string][]LeadTimeSample{}
	var all []LeadTimeSample
	for _, s := range samples {
		if !s.valid() {
			continue
		}
		all = append(all, s)
		if s.VendorID != "" {
			byVendor[s.VendorID] = append(byVendor[s.VendorID], s)
		}
	}

	out := Result{Overall: statsOf(all), ByVendor: make(map[string]LeadTimeStats, len(byVendor))}
	for vendorID, vs := range byVendor {
		out.ByVendor[vendorID] = statsOf(vs)
	}
	return out
}

// statsOf builds one LeadTimeStats over already-validated samples.
func statsOf(samples []LeadTimeSample) LeadTimeStats {
	stats := LeadTimeStats{N: len(samples)}
	if len(samples) < MinSamples {
		return stats
	}

	days := make([]float64, len(samples))
	for i, s := range samples {
		days[i] = s.LeadTimeDays()
	}
	// ok is always true here: len(days) >= MinSamples > 0 and both q
	// values are in range.
	stats.P50Days, _ = Quantile(days, 0.5)
	stats.P90Days, _ = Quantile(days, 0.9)

	perStage := map[string][]float64{}
	for _, s := range samples {
		for stage, d := range s.StageDurations {
			perStage[stage] = append(perStage[stage], d)
		}
	}
	for stage, ds := range perStage {
		if len(ds) < MinSamples {
			continue // same no-fabrication rule, per stage
		}
		if stats.StageMedianDays == nil {
			stats.StageMedianDays = map[string]float64{}
		}
		stats.StageMedianDays[stage], _ = Quantile(ds, 0.5)
	}
	return stats
}
