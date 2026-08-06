package forecast

// QualitySample is one GoodsReceiptLine's recorded inspection outcome
// (uc-infra#82): qty_accepted/qty_rejected are optional fields on
// GoodsReceiptLine (Version 2) — most lines, until a tenant starts using
// them, have neither set. HasData distinguishes "this line has no
// quality record at all" (the common case) from "this line's quality
// record is 0 accepted / 0 rejected" (a real, if degenerate, outcome —
// only reachable when qty_received is itself 0, since
// purchasing.validateGoodsReceiptLineQuality requires the two to net) —
// neither QtyAccepted nor QtyRejected being non-zero is sufficient on
// its own to mean "has data," the same reason OnTimeSample can't use a
// zero time.Time as its own "no promise" sentinel.
//
// Unlike OnTimeSample (one sample per completed PurchaseOrder), a
// sample here is one GoodsReceiptLine — a single PO can contribute many
// quality samples, one per line, each judged independently. Callers
// resolve VendorID (via GoodsReceipt -> PurchaseOrder.vendor_id) the
// same division-of-responsibility this package's every other sample
// type already follows: this package doesn't care where a vendor id
// came from, only whether one was resolvable.
type QualitySample struct {
	VendorID    string
	QtyAccepted float64
	QtyRejected float64
	HasData     bool
}

// valid reports whether this sample carries real, countable quality
// data: HasData, non-negative quantities, and a nonzero inspected total
// (QtyAccepted+QtyRejected > 0). That last condition is not redundant
// with HasData (independent review of uc-infra#82: an earlier draft let
// a 0/0 line — HasData=true, both quantities legitimately zero — count
// toward N with nothing in either total, so QualityStats.Rate()'s
// divide-by-zero guard silently reported 0%, i.e. "100% rejected," for a
// line that inspected NOTHING). A 0/0 sample is real data (it means the
// receipt itself was a zero-quantity line — the only way
// purchasing.validateGoodsReceiptLineQuality's netting invariant allows
// 0/0 through, since it requires qty_accepted+qty_rejected==qty_received)
// but it has no quality VERDICT to contribute, the same way a
// LeadTimeSample or OnTimeSample with no usable date pair contributes
// nothing rather than a fabricated zero. Excluding it here, not just at
// render time, keeps Sufficient()/Rate() a single trustworthy source
// of truth: N only ever counts lines that could have shifted the rate.
// A negative quantity is a second, independent reason to exclude a
// sample: purchasing.validateGoodsReceiptLineQuality already rejects a
// negative qty_accepted/qty_rejected at write time, so one reaching here
// would mean corrupt/pre-validation data (a CSV import bypassing the
// hook, say) — the same defensive posture LeadTimeSample.valid() takes
// toward reversed timestamps it also shouldn't normally see.
func (s QualitySample) valid() bool {
	return s.HasData && s.QtyAccepted >= 0 && s.QtyRejected >= 0 && s.QtyAccepted+s.QtyRejected > 0
}

// QualityStats is the aggregate over a set of quality samples. Unlike
// OnTimeStats (a simple per-order pass/fail rate), Rate here is
// quantity-weighted — sum(accepted) / sum(accepted+rejected) — not an
// average of each line's own rate, so one large defective shipment
// can't be diluted to the same weight as a single-unit perfect line.
// N counts SAMPLES (lines with quality data), which is what Sufficient
// gates on — the no-fabrication rule is about having enough independent
// observations to trust a rate, not about how many units passed
// through them.
type QualityStats struct {
	N             int
	AcceptedTotal float64
	RejectedTotal float64
}

// Sufficient reports whether enough independent lines have quality data
// for Rate to be trustworthy (N >= MinSamples), the same MinSamples
// discipline LeadTimeStats/OnTimeStats already apply to their own
// aggregates.
func (s QualityStats) Sufficient() bool {
	return s.N >= MinSamples
}

// Rate is the fraction of total inspected quantity that was accepted,
// in [0, 1]. Callers must check Sufficient() first — see QualityStats's
// own doc comment for why this is quantity- not sample-weighted.
func (s QualityStats) Rate() float64 {
	total := s.AcceptedTotal + s.RejectedTotal
	if total == 0 {
		return 0
	}
	return s.AcceptedTotal / total
}

// QualityResult is ComputeQuality's output: overall stats across every
// GoodsReceiptLine with quality data, plus a per-vendor breakdown keyed
// by VendorID — same shape as Result/OnTimeResult.
type QualityResult struct {
	Overall  QualityStats
	ByVendor map[string]QualityStats
}

// ComputeQuality aggregates samples into overall + per-vendor quality
// stats. Samples with no quality data (see QualitySample.valid) are
// skipped entirely — expected to be the majority of a tenant's
// GoodsReceiptLines, since qty_accepted/qty_rejected are optional and
// every line written before uc-infra#82 has neither. Deterministic:
// same samples in any order produce identical output (summation order
// of IEEE-754 floats is technically not perfectly order-independent,
// but the same rounding-noise disclaimer already applies to every other
// float sum in this package's own Compute/ComputeOnTime; no test in
// this package relies on bit-exact reproducibility across orderings,
// only on the same rounded, human-facing result).
func ComputeQuality(samples []QualitySample) QualityResult {
	byVendor := map[string][]QualitySample{}
	var all []QualitySample
	for _, s := range samples {
		if !s.valid() {
			continue
		}
		all = append(all, s)
		if s.VendorID != "" {
			byVendor[s.VendorID] = append(byVendor[s.VendorID], s)
		}
	}

	out := QualityResult{Overall: qualityStatsOf(all), ByVendor: make(map[string]QualityStats, len(byVendor))}
	for vendorID, vs := range byVendor {
		out.ByVendor[vendorID] = qualityStatsOf(vs)
	}
	return out
}

// qualityStatsOf builds one QualityStats over already-validated samples.
func qualityStatsOf(samples []QualitySample) QualityStats {
	stats := QualityStats{N: len(samples)}
	for _, s := range samples {
		stats.AcceptedTotal += s.QtyAccepted
		stats.RejectedTotal += s.QtyRejected
	}
	return stats
}
