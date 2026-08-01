package forecast

import "time"

// OnTimeSample is one completed purchase order's promise-vs-actual
// delivery outcome (#11): the vendor's own commitment (PurchaseOrder.
// promised_delivery_date, set at order time) against the same
// ReceivedDate LeadTimeSample already uses. Both fields are the
// caller's job to resolve — this package doesn't care where they came
// from, same division of responsibility as LeadTimeSample.
type OnTimeSample struct {
	VendorID     string
	PromisedDate time.Time
	ReceivedDate time.Time
}

// OnTime reports whether this sample arrived at or before its promise.
// The boundary is inclusive: arriving exactly on the promised date
// meets the promise, it doesn't miss it.
func (s OnTimeSample) OnTime() bool {
	return !s.ReceivedDate.After(s.PromisedDate)
}

// valid reports whether this sample is usable: both a promise and a
// receipt date are present. Unlike LeadTimeSample.valid, a promise that
// falls AFTER the receipt date is deliberately NOT dropped here — that
// is not reversed noise, it's an early delivery, still a real on-time
// outcome. A promise that falls before the ORDER date can't reach this
// package at all (entity.Field's NotBefore already rejects it at write
// time), so no equivalent chronology guard is needed on that end.
func (s OnTimeSample) valid() bool {
	return !s.PromisedDate.IsZero() && !s.ReceivedDate.IsZero()
}

// OnTimeStats is the aggregate over a set of on-time samples. Rate is
// only meaningful when Sufficient() — callers must render an
// insufficient-history state otherwise, same MinSamples discipline
// LeadTimeStats already applies to quantiles, applied here to a rate
// instead.
type OnTimeStats struct {
	N       int
	OnTimeN int
}

// Sufficient reports whether enough history exists for Rate to be
// trustworthy (N >= MinSamples).
func (s OnTimeStats) Sufficient() bool {
	return s.N >= MinSamples
}

// Rate is the fraction of samples that arrived on or before their
// promised date, in [0, 1]. Callers must check Sufficient() first: on
// an insufficient N this is either a divide-by-zero-guarded 0 (N == 0)
// or a real ratio computed from too few observations to trust — neither
// is a number a report may render as a vendor's on-time rate.
func (s OnTimeStats) Rate() float64 {
	if s.N == 0 {
		return 0
	}
	return float64(s.OnTimeN) / float64(s.N)
}

// OnTimeResult is ComputeOnTime's output: overall stats across every
// completed order with a resolvable promise, plus a per-vendor
// breakdown keyed by VendorID — same shape as Result.
//
// Scope limitation callers must disclose, not paper over (independent
// review of #11, 2026-07-31): a sample only exists for a PO this package
// was TOLD was completed. A PO that never arrived at all — the worst
// possible outcome — is simply absent from the input, not counted as a
// miss, so a chronically non-delivering vendor's rate is computed only
// over the orders that DID eventually show up and can read as
// deceptively high (10 promised, 8 never arrived, 2 arrived on time ->
// this package reports 100%, not 20%). Same "completed orders only"
// scope #30's lead-time table already has; on-time turns that scope into
// a number that can be misread as a vendor's overall reliability, so it
// needs the caller's own explicit disclosure (see reporting.go's
// buildOnTimeRows / the "Orders Received" column header), the same way
// buildReorderSignals discloses "(all suppliers)" rather than letting a
// borrowed statistic pass as vendor-specific.
type OnTimeResult struct {
	Overall  OnTimeStats
	ByVendor map[string]OnTimeStats
}

// ComputeOnTime aggregates samples into overall + per-vendor on-time
// stats. Samples without both a promise and a receipt date (see
// OnTimeSample.valid) are skipped entirely — expected to be the
// majority of a tenant's completed orders, since promised_delivery_date
// is optional and every PO written before #11 has none; that is not a
// data-quality problem this function needs to flag, just fewer usable
// samples. Deterministic: same samples in any order produce identical
// output.
func ComputeOnTime(samples []OnTimeSample) OnTimeResult {
	byVendor := map[string][]OnTimeSample{}
	var all []OnTimeSample
	for _, s := range samples {
		if !s.valid() {
			continue
		}
		all = append(all, s)
		if s.VendorID != "" {
			byVendor[s.VendorID] = append(byVendor[s.VendorID], s)
		}
	}

	out := OnTimeResult{Overall: onTimeStatsOf(all), ByVendor: make(map[string]OnTimeStats, len(byVendor))}
	for vendorID, vs := range byVendor {
		out.ByVendor[vendorID] = onTimeStatsOf(vs)
	}
	return out
}

// onTimeStatsOf builds one OnTimeStats over already-validated samples.
func onTimeStatsOf(samples []OnTimeSample) OnTimeStats {
	stats := OnTimeStats{N: len(samples)}
	for _, s := range samples {
		if s.OnTime() {
			stats.OnTimeN++
		}
	}
	return stats
}
