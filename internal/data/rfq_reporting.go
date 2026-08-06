package data

import (
	"context"
	"fmt"

	"github.com/universaltill/universal-core/internal/kernel/money"
)

// moneyMinorUnitsPattern guards the unit_price cast below the same way
// uuidPattern (reporting.go) guards a ::uuid cast: entity.ValidateRecord
// requires a FieldMoney value be a whole number, but it validates only
// values submitted through the validated write path — a row written
// before RequestForQuotationQuoteLine's Version 1->2 bump (unit_price
// was FieldNumber, a major-unit decimal like "9.5") still has a
// fractional stored value until cmd/backfill-quote-line-unit-price
// converts it (uc-infra#68's version-bump/backfill pair). Postgres's
// ::bigint cast raises a hard error on "9.5" — without this guard that
// aborts the WHOLE comparison report, not just this one vendor's quote
// (independent review of uc-infra#68's own first pass, reproduced
// directly against Postgres: `select ('9.5')::bigint` errors). Matching
// uuidPattern's own "one bad row is silently excluded, not fatal"
// resolution: a quote row whose unit_price isn't a plain integer string
// is excluded from the grid (that vendor renders as not having quoted
// this line) rather than 500ing the whole page.
const moneyMinorUnitsPattern = `^-?[0-9]+$`

// RFQComparisonLine is one line item on a RequestForQuotation's
// vendor-comparison grid: the requested item + qty, plus each invited
// vendor's quoted unit price for this specific line, keyed by vendor
// Party id. A vendor absent from QuotesByVendor simply never quoted this
// line — that's a real, meaningful gap the report must show as a blank,
// not a fabricated zero (see RFQComparison's own doc comment).
//
// QuotesByVendor is money.Money (minor units), not float64 (uc-infra#68)
// — RequestForQuotationQuoteLine.unit_price is a FieldMoney field now,
// and this is the exact map this report's own footer total sums, so
// keeping it in minor units all the way through is what makes that sum
// exact int64 addition instead of the float64 accumulation that produced
// a visible IEEE artifact (0.1 + 0.2 = 0.30000000000000004) in the
// originating review.
type RFQComparisonLine struct {
	ID             string
	ItemName       string
	Qty            float64
	QuotesByVendor map[string]money.Money
}

// RFQComparisonVendor is one vendor invited to quote against a
// RequestForQuotation — the comparison grid's column headers.
type RFQComparisonVendor struct {
	ID   string
	Name string
}

// RFQComparison assembles the vendor-comparison grid for one
// RequestForQuotation (rfqID): every requested line (ordered by
// created_at, then id for a deterministic tie-break — no ordinal field
// exists on RequestForQuotationLine, same "creation order is display
// order" convention POLine/GoodsReceiptLine's own master-detail sections
// already rely on) crossed against every invited vendor's quoted price
// for that line, where one was actually recorded.
//
// Three separate queries, assembled into the map shape in Go rather than
// one join with GROUP BY: RequestForQuotationQuoteLine rows are sparse
// by design (a vendor need not quote every line — that's the whole point
// of surfacing the gap), and a single query can't produce "one column
// per vendor" for a variable, per-RFQ number of invited vendors. Same
// "assemble the map in Go, not SQL" shape
// OnOrderQtyByItem/OnHandQtyByItem/LatestPOVendorByItem already use for
// their own per-item lookups.
//
// Every ::uuid cast below is guarded by uuidPattern (reporting.go's
// package-level const): a malformed or dangling reference on any one row
// — a bad item_id on a line, a bad vendor_id on an invited vendor or a
// quote line — excludes just that row from its join, never aborts the
// whole report, same discipline as every other query in this package.
// The rfqID filter itself (which line/vendor/quote row belongs to THIS
// RFQ) is a plain text comparison against the stored parent-reference
// field, not a cast — matching CompletedPOLeadTimes' own vendor-join
// reasoning: a row whose parent reference doesn't textually equal rfqID
// simply doesn't match, malformed or not, so no separate guard is needed
// on that particular comparison. (Known, package-wide limitation of that
// convention: a parent reference stored in a non-canonical but castable
// UUID spelling — uppercase hex, say — is textually unequal and so drops
// out of the report entirely. Nothing in this kernel normalizes
// FieldReference values; fixing it belongs at write time or as one
// change across every query in this package, not here alone.)
func (r *ReportingRepo) RFQComparison(ctx context.Context, rfqID string) ([]RFQComparisonLine, []RFQComparisonVendor, error) {
	lineRows, err := r.db.QueryContext(ctx,
		`SELECT l.id, coalesce(i.data->>'name', i.id::text), coalesce((l.data->>'qty')::numeric, 0)
		 FROM records l
		 JOIN records i
		   ON i.entity_type = 'Item'
		  AND i.deleted_at IS NULL
		  AND i.id = (l.data->>'item_id')::uuid
		 WHERE l.entity_type = 'RequestForQuotationLine'
		   AND l.deleted_at IS NULL
		   AND l.data->>'request_for_quotation_id' = $1
		   AND l.data->>'item_id' ~ $2
		 ORDER BY l.created_at, l.id`,
		rfqID, uuidPattern,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("rfq comparison lines: %w", err)
	}
	defer lineRows.Close()

	var lines []RFQComparisonLine
	for lineRows.Next() {
		var line RFQComparisonLine
		if err := lineRows.Scan(&line.ID, &line.ItemName, &line.Qty); err != nil {
			return nil, nil, fmt.Errorf("scan rfq comparison line row: %w", err)
		}
		lines = append(lines, line)
	}
	if err := lineRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rfq comparison lines: %w", err)
	}

	vendorRows, err := r.db.QueryContext(ctx,
		`SELECT p.id, coalesce(p.data->>'name', p.id::text)
		 FROM records rv
		 JOIN records p
		   ON p.entity_type = 'Party'
		  AND p.deleted_at IS NULL
		  AND p.id = (rv.data->>'vendor_id')::uuid
		 WHERE rv.entity_type = 'RequestForQuotationVendor'
		   AND rv.deleted_at IS NULL
		   AND rv.data->>'request_for_quotation_id' = $1
		   AND rv.data->>'vendor_id' ~ $2
		 ORDER BY p.data->>'name', p.id`,
		rfqID, uuidPattern,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("rfq comparison vendors: %w", err)
	}
	defer vendorRows.Close()

	var vendors []RFQComparisonVendor
	for vendorRows.Next() {
		var v RFQComparisonVendor
		if err := vendorRows.Scan(&v.ID, &v.Name); err != nil {
			return nil, nil, fmt.Errorf("scan rfq comparison vendor row: %w", err)
		}
		vendors = append(vendors, v)
	}
	if err := vendorRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rfq comparison vendors: %w", err)
	}

	// rfq_line_id is cast (guarded) to join RequestForQuotationLine — the
	// hop that scopes a quote line to THIS rfq's own lines, since a quote
	// line carries no request_for_quotation_id of its own (it's not a
	// composition child — see RequestForQuotationQuoteLine's own doc
	// comment). vendor_id is guarded and cast for the same reason, even
	// though nothing is joined through it: both values become map KEYS
	// that must compare equal to an id read straight out of the uuid
	// `id` column (RFQComparisonLine.ID / RFQComparisonVendor.ID), and
	// entity.FieldReference validates only "is a string" — it never
	// normalizes UUID spelling. A reference stored in any non-canonical
	// but castable form (uppercase hex, braces, unhyphenated) joins fine
	// yet would produce a key that matches nothing, silently dropping a
	// real quoted price from the grid and rendering that vendor as
	// "never quoted" (independent review, 2026-07-31 — reproduced with an
	// uppercase rfq_line_id/vendor_id, see rfq_reporting_test.go's
	// non-canonical-reference case). Round-tripping through ::uuid makes
	// both keys canonical, exactly like the id column they're matched
	// against. A malformed (uncastable) vendor_id now excludes its quote
	// row rather than producing an unmatchable key — the same invisible
	// outcome, one query stage earlier.
	//
	// ORDER BY created_at, id: nothing constrains a tenant to one quote
	// row per (line, vendor), and the map assembled below is last-write-
	// wins, so without an explicit order the rendered price for a
	// re-quoted line would depend on Postgres's scan order. Ordering
	// makes "the most recently recorded quote wins" a real, stable rule.
	quoteRows, err := r.db.QueryContext(ctx,
		`SELECT l.id, (q.data->>'vendor_id')::uuid, coalesce((q.data->>'unit_price')::bigint, 0)
		 FROM records q
		 JOIN records l
		   ON l.entity_type = 'RequestForQuotationLine'
		  AND l.deleted_at IS NULL
		  AND l.id = (q.data->>'rfq_line_id')::uuid
		 WHERE q.entity_type = 'RequestForQuotationQuoteLine'
		   AND q.deleted_at IS NULL
		   AND q.data->>'rfq_line_id' ~ $2
		   AND q.data->>'vendor_id' ~ $2
		   AND q.data->>'unit_price' ~ $3
		   AND l.data->>'request_for_quotation_id' = $1
		 ORDER BY q.created_at, q.id`,
		rfqID, uuidPattern, moneyMinorUnitsPattern,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("rfq comparison quotes: %w", err)
	}
	defer quoteRows.Close()

	byLine := map[string]map[string]money.Money{}
	for quoteRows.Next() {
		var lineID, vendorID string
		var price int64
		if err := quoteRows.Scan(&lineID, &vendorID, &price); err != nil {
			return nil, nil, fmt.Errorf("scan rfq comparison quote row: %w", err)
		}
		m, ok := byLine[lineID]
		if !ok {
			m = map[string]money.Money{}
			byLine[lineID] = m
		}
		m[vendorID] = money.Money(price)
	}
	if err := quoteRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rfq comparison quotes: %w", err)
	}

	for i := range lines {
		lines[i].QuotesByVendor = byLine[lines[i].ID]
	}

	return lines, vendors, nil
}
