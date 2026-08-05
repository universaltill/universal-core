package api

import (
	"net/url"
	"strings"
	"testing"
)

// TestListPage_FilterAcceptsRegionalNumber is board #74's headline
// behaviour: a viewer who types back exactly what a FieldNumber cell
// shows them — grouping separators, decimal separator, digit shapes,
// all of it — gets the matching row, not zero results. Same seeded
// Shipment (weight 1234567.5) TestListPage_RegionalDateAndNumberFormatting
// already renders per region; this filters by what that render produces.
func TestListPage_FilterAcceptsRegionalNumber(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	for name, tc := range map[string]struct {
		query string
	}{
		"british grouping":   {"?filter=weight&q=" + url.QueryEscape("1,234,567.5")},
		"american grouping":  {"?region=en-US&filter=weight&q=" + url.QueryEscape("1,234,567.5")},
		"turkish separators": {"?lang=tr&region=tr-TR&filter=weight&q=" + url.QueryEscape("1.234.567,5")},
		"arabic digits":      {"?lang=ar&region=ar-AE&filter=weight&q=" + url.QueryEscape("١٬٢٣٤٬٥٦٧٫٥")},
		"persian digits":     {"?lang=fa&region=fa-IR&filter=weight&q=" + url.QueryEscape("۱٬۲۳۴٬۵۶۷٫۵")},
		// Indian lakh/crore grouping — the one non-uniform grouping
		// pattern this kernel ships, and previously covered only by
		// locale_test.go's unit tests (independent review), not by
		// anything hitting the actual filter/query path.
		"indian grouping": {"?region=en-IN&filter=weight&q=" + url.QueryEscape("12,34,567.5")},
	} {
		t.Run(name, func(t *testing.T) {
			body := getList(t, mux, tenantID, tc.query).Body.String()
			if !strings.Contains(body, "Container 1") {
				t.Errorf("regional-formatted filter %q found no match\n%s", tc.query, excerpt(body))
			}
		})
	}
}

// TestListPage_FilterAcceptsRegionalDate is the date half of the same
// fix, including the Jalali calendar case (a genuinely different
// calendar, not just a different script/order).
func TestListPage_FilterAcceptsRegionalDate(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	for name, tc := range map[string]struct {
		query string
	}{
		"british day-first":    {"?filter=ship_date&q=" + url.QueryEscape("03/04/2026")},
		"american month-first": {"?region=en-US&filter=ship_date&q=" + url.QueryEscape("04/03/2026")},
		"turkish dots":         {"?lang=tr&region=tr-TR&filter=ship_date&q=" + url.QueryEscape("03.04.2026")},
		"arabic digits":        {"?lang=ar&region=ar-AE&filter=ship_date&q=" + url.QueryEscape("٠٣/٠٤/٢٠٢٦")},
		"jalali":               {"?lang=fa&region=fa-IR&filter=ship_date&q=" + url.QueryEscape("۱۴۰۵/۰۱/۱۴")},
	} {
		t.Run(name, func(t *testing.T) {
			body := getList(t, mux, tenantID, tc.query).Body.String()
			if !strings.Contains(body, "Container 1") {
				t.Errorf("regional-formatted date filter %q found no match\n%s", tc.query, excerpt(body))
			}
		})
	}
}

// TestListPage_FilterRegionalDate_AmbiguousOrderDoesNotFalseMatch pairs
// a negative and a positive check on the SAME literal query text, which
// is what actually proves the fix applies the ACTIVE locale's field
// order rather than accepting any plausible date shape — a negative
// check alone (independent review) can't distinguish "the locale's day/
// month order was actually applied" from "the whole mechanism does
// nothing and nothing ever matches": "04/03/2026" is the seeded date
// under en-US's month-first order (must match), but under en-GB's
// day-first order the identical text means 4 March, a different day
// (must not match).
func TestListPage_FilterRegionalDate_AmbiguousOrderDoesNotFalseMatch(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	gb := getList(t, mux, tenantID, "?filter=ship_date&q="+url.QueryEscape("04/03/2026")).Body.String()
	if strings.Contains(gb, "Container 1") {
		t.Errorf("en-GB day-first filter %q must not match a stored 2026-04-03 record\n%s",
			"04/03/2026", excerpt(gb))
	}

	us := getList(t, mux, tenantID, "?region=en-US&filter=ship_date&q="+url.QueryEscape("04/03/2026")).Body.String()
	if !strings.Contains(us, "Container 1") {
		t.Errorf("en-US month-first filter %q should match the stored 2026-04-03 record — if this also fails, "+
			"the negative check above proves nothing\n%s", "04/03/2026", excerpt(us))
	}
}

// TestListPage_FilterUnparseableRegionalValue_FallsBackHonestly: input
// that doesn't parse under the active locale at all must degrade to the
// pre-#74 literal-text match (finding nothing, since it isn't the raw
// stored text either) rather than erroring the page or matching
// everything.
func TestListPage_FilterUnparseableRegionalValue_FallsBackHonestly(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	for name, tc := range map[string]struct{ field, q string }{
		"garbage number": {"weight", "not a number"},
		"garbage date":   {"ship_date", "not a date"},
	} {
		t.Run(name, func(t *testing.T) {
			q := "?filter=" + tc.field + "&q=" + url.QueryEscape(tc.q)
			body := getList(t, mux, tenantID, q).Body.String()
			if strings.Contains(body, "Container 1") {
				t.Errorf("unparseable filter %q should not match\n%s", q, excerpt(body))
			}
		})
	}
}

// TestListPage_FilterAsciiNumber_MisreadUnderOtherRegion_FallsBackToRawMatch
// is the specific case validGrouping's "check before stripping" design
// exists for: tr-TR's group separator is the SAME character as en-GB's
// decimal separator, so plain ASCII "1234567.5" cannot be validly
// interpreted as tr-TR grouping — ParseNumber correctly refuses it
// (locale_test.go's own unit coverage) — but the filter must still work
// because the fallback to the raw literal text still finds the
// literally-ASCII-stored value, same as it always did pre-#74.
func TestListPage_FilterAsciiNumber_MisreadUnderOtherRegion_FallsBackToRawMatch(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	body := getList(t, mux, tenantID, "?lang=tr&region=tr-TR&filter=weight&q="+url.QueryEscape("1234567.5")).
		Body.String()
	if !strings.Contains(body, "Container 1") {
		t.Errorf("plain ASCII filter under tr-TR should still fall back to a literal match\n%s", excerpt(body))
	}
}

// TestListPage_FilterBox_ShowsRawTypedValue: the filter box and the
// sort/page links must echo back exactly what the viewer typed, not the
// normalized ASCII/ISO form ParseNumber/ParseDate produce internally —
// otherwise the visible search box would silently rewrite itself on
// every redisplay.
func TestListPage_FilterBox_ShowsRawTypedValue(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	raw := "1.234.567,5"
	body := getList(t, mux, tenantID, "?lang=tr&region=tr-TR&filter=weight&q="+url.QueryEscape(raw)).Body.String()
	if !strings.Contains(body, `value="`+raw+`"`) {
		t.Errorf("filter box did not echo back the raw typed value %q\n%s", raw, excerpt(body))
	}
	if strings.Contains(body, `value="1234567.5"`) {
		t.Error("filter box shows the normalized ASCII form instead of what was typed")
	}
	// The href that carries the filter across a sort click must also
	// preserve the raw text (URL-encoded), not the normalized one.
	if !strings.Contains(body, url.QueryEscape(raw)) {
		t.Errorf("sort/page links dropped the raw filter value %q\n%s", raw, excerpt(body))
	}
}
