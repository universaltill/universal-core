package api

import (
	"html"
	"net/url"
	"regexp"
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

// regionPickerBlock isolates the <select class="uc-nav-region">...</select>
// markup from a full page body, so assertions about the region picker's
// own hrefs can't accidentally pass or fail on an unrelated "q="/"filter="
// elsewhere on the page (the sort/page links right beside the filter box,
// for instance, are SUPPOSED to carry "q=").
var regionPickerBlock = regexp.MustCompile(`(?s)<select class="uc-nav-region".*?</select>`)

// regionOptionValue extracts the href="..." attribute value.Query() maps
// from a single <option value="..."> — parsed properly (unescaped, then
// url.Parse'd) rather than by substring, so an assertion can check "is
// -q- present"/"does filter= equal weight" independent of the query
// string's key order (url.Values.Encode() sorts keys alphabetically,
// which shifts whenever a param is added — a brittle thing to hardcode)
// and isn't fooled by an unrelated key that happens to end in "q"
// (uc-infra#128's independent review, F7).
var regionOptionAttr = regexp.MustCompile(`<option value="([^"]*)"`)

// regionOptionValues maps each region option's tag to its href's parsed
// query values, reading the tag back out of the href itself (?region=...)
// rather than assuming option order matches regionChoices.
func regionOptionValues(t *testing.T, body string) map[string]url.Values {
	t.Helper()
	picker := regionPickerBlock.FindString(body)
	if picker == "" {
		t.Fatalf("region picker not found in rendered page\n%s", excerpt(body))
	}
	out := map[string]url.Values{}
	for _, m := range regionOptionAttr.FindAllStringSubmatch(picker, -1) {
		href := html.UnescapeString(m[1])
		u, err := url.Parse(href)
		if err != nil {
			t.Fatalf("region option href %q did not parse: %v", href, err)
		}
		vals := u.Query()
		tag := vals.Get("region")
		if tag == "" {
			t.Fatalf("region option href %q has no region= param", href)
		}
		out[tag] = vals
	}
	if len(out) == 0 {
		t.Fatalf("no <option value=...> found in region picker\n%s", picker)
	}
	return out
}

// TestRegionPicker_DropsFilterValueAcrossRegionSwitch is uc-infra#128: a
// FieldNumber/FieldDate filter's "q" holds RAW text typed under the
// PREVIOUS region's rules (board #74), and re-parsing that same raw text
// under a newly-picked region silently changes (or breaks) what it
// matches — the viewer never asked for that, and nothing on the page
// says it happened. The fix is that the region picker's own links must
// never carry "q" across a region switch, while still carrying "filter"
// (which FIELD is targeted has no locale dependence, and dropping it too
// would regress the already-fixed bookmarked-"?filter=weight" case,
// uc-infra#129).
//
// This only exercises the "stops matching" half of the bug (grouping
// that fails to parse under the new region falls back to a literal-text
// match, per TestListPage_FilterAsciiNumber_MisreadUnderOtherRegion_
// FallsBackToRawMatch, and finds nothing) — see
// TestRegionPicker_DropsFilterValueAcrossRegionSwitch_ReinterpretsDate
// below for the sharper "matches a DIFFERENT record, silently" half the
// issue title actually names.
func TestRegionPicker_DropsFilterValueAcrossRegionSwitch(t *testing.T) {
	tenantID, mux := setupRegionalTenantTwoRows(t)

	// "en" is the language whose picker actually offers more than one
	// region (regionChoices["tr"]/["fa"] etc. mostly don't, so a
	// same-language, different-region pair is needed to reach the picker
	// at all — en-GB and en-IN disagree on grouping, en-GB/British
	// comma-grouped vs en-IN's lakh/crore, the same pair
	// TestListPage_FilterAcceptsRegionalNumber already exercises).
	raw := "1,234,567.5"
	body := getList(t, mux, tenantID, "?filter=weight&q="+url.QueryEscape(raw)).Body.String()
	if !strings.Contains(body, "Container 1") || strings.Contains(body, "Container 2") {
		t.Fatalf("sanity: the default en-GB filter should match only Container 1\n%s", excerpt(body))
	}

	options := regionOptionValues(t, body)
	enIN, ok := options["en-IN"]
	if !ok {
		t.Fatalf("expected an en-IN region option, got tags: %v", options)
	}
	if enIN.Has("q") {
		t.Errorf("region picker's en-IN option carries \"q\" across a region switch — it will be reinterpreted under en-IN's rules: %v", enIN)
	}
	if enIN.Get("filter") != "weight" {
		t.Errorf("region picker's en-IN option dropped \"filter=weight\" — the field selector should survive a region switch even though \"q\" doesn't: %v", enIN)
	}

	// Follow the picker's own en-IN link (a plain field-selected, empty
	// filter — not the stale-text URL the picker used to produce) and
	// confirm it does NOT silently show zero rows: an active-but-empty
	// filter finds everything (BOTH rows, not coincidentally just the one
	// the stale filter used to match), the same already-tested behaviour
	// as a bookmarked "?filter=weight" with no "q" yet (uc-infra#129).
	afterSwitch := getList(t, mux, tenantID, "?"+enIN.Encode()).Body.String()
	if !strings.Contains(afterSwitch, "Container 1") || !strings.Contains(afterSwitch, "Container 2") {
		t.Errorf("following the region picker's own en-IN link should show every row (filter=weight with no q) got:\n%s", excerpt(afterSwitch))
	}
	if strings.Contains(afterSwitch, `value="`+raw+`"`) {
		t.Error("the filter box still shows the stale en-GB-formatted text after switching region")
	}

	// Negative control confirming this test would actually have caught
	// the bug: the OLD (buggy) picker href — carrying the stale "q" into
	// the new region — silently misreads British "1,234,567.5" under
	// en-IN's lakh/crore grouping rules and finds nothing.
	stale := getList(t, mux, tenantID, "?filter=weight&region=en-IN&q="+url.QueryEscape(raw)).Body.String()
	if strings.Contains(stale, "Container 1") {
		t.Fatalf("test setup problem: carrying the stale British-grouped q into en-IN should NOT match (proves the bug is real) — got a match, so this test can't distinguish fixed from broken\n%s", excerpt(stale))
	}
}

// TestRegionPicker_DropsFilterValueAcrossRegionSwitch_ReinterpretsDate is
// uc-infra#128's independent review, F1: the weight-grouping test above
// only proves a region switch can make a filter STOP matching (a parse
// failure falling back to literal-text, matching nothing) — a real but
// weaker failure mode than the issue title's actual claim, "silently
// reinterprets". This proves the sharper case: the exact same raw text
// matches a DIFFERENT record after a region switch, with nothing on the
// page to say so, using en-GB (day-first) vs en-US (month-first) reading
// the identical string "03/04/2026" as two different real dates.
func TestRegionPicker_DropsFilterValueAcrossRegionSwitch_ReinterpretsDate(t *testing.T) {
	tenantID, mux := setupAmbiguousDateTenant(t)
	raw := "03/04/2026"

	// Default en-GB, day-first: "03/04/2026" is 3 April — AprilThird.
	body := getList(t, mux, tenantID, "?filter=ship_date&q="+url.QueryEscape(raw)).Body.String()
	if !strings.Contains(body, "AprilThird") || strings.Contains(body, "MarchFourth") {
		t.Fatalf("sanity: en-GB day-first %q should match only AprilThird\n%s", raw, excerpt(body))
	}

	options := regionOptionValues(t, body)
	enUS, ok := options["en-US"]
	if !ok {
		t.Fatalf("expected an en-US region option, got tags: %v", options)
	}
	if enUS.Has("q") {
		t.Errorf("region picker's en-US option carries \"q\" — following it would silently reinterpret %q as 4 March instead of 3 April: %v", raw, enUS)
	}

	// Following the picker's own (fixed) en-US link finds everything —
	// no filter is active, not a different, silently-matched record.
	afterSwitch := getList(t, mux, tenantID, "?"+enUS.Encode()).Body.String()
	if !strings.Contains(afterSwitch, "AprilThird") || !strings.Contains(afterSwitch, "MarchFourth") {
		t.Errorf("following the region picker's own en-US link should show every row, got:\n%s", excerpt(afterSwitch))
	}

	// Negative control: this is what following the OLD (buggy) picker
	// href would have done — the identical raw text, now read under
	// en-US's month-first rules, silently matches MarchFourth instead of
	// the AprilThird record the viewer actually had filtered to. No
	// error, no empty page — just a different answer.
	stale := getList(t, mux, tenantID, "?filter=ship_date&region=en-US&q="+url.QueryEscape(raw)).Body.String()
	if !strings.Contains(stale, "MarchFourth") || strings.Contains(stale, "AprilThird") {
		t.Fatalf("test setup problem: %q under en-US should match MarchFourth, not AprilThird (proves the reinterpretation is real) — got:\n%s", raw, excerpt(stale))
	}
}

// TestRegionPicker_DropsTextFilterValueToo pins uc-infra#128's
// independent review finding F2: the picker drops "q" unconditionally,
// even for a FieldString filter (no locale dependence at all — "q" here
// can never be reinterpreted) and even between two regions that format
// identically (en-GB/en-IE agree on every rule this suite exercises).
// regionOptions has no Definition to check the active field's type
// against and no way to compare two regions' rules without one (see its
// own doc comment) — so this is a deliberate, documented over-clear
// rather than an oversight, and this test exists to PIN that choice, not
// to demand it change.
func TestRegionPicker_DropsTextFilterValueToo(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	body := getList(t, mux, tenantID, "?filter=name&q=Container").Body.String()
	options := regionOptionValues(t, body)
	enIE, ok := options["en-IE"]
	if !ok {
		t.Fatalf("expected an en-IE region option, got tags: %v", options)
	}
	if enIE.Has("q") {
		t.Errorf("region picker's en-IE option carries \"q\" for a plain FieldString filter — this field has no locale dependence, so this documents an intentionally over-broad clear, not an expected miss: %v", enIE)
	}
	if enIE.Get("filter") != "name" {
		t.Errorf("region picker's en-IE option dropped \"filter=name\": %v", enIE)
	}
}

// TestRegionPicker_PreservesSortAndPage guards the part of the original
// design (board #22) this fix must NOT regress: sort order and page
// position are locale-invariant, so — unlike "q" — they still ride along
// across a region switch.
func TestRegionPicker_PreservesSortAndPage(t *testing.T) {
	tenantID, mux := setupRegionalTenant(t)

	body := getList(t, mux, tenantID, "?sort=weight&dir=desc&page=1").Body.String()
	options := regionOptionValues(t, body)
	enUS, ok := options["en-US"]
	if !ok {
		t.Fatalf("expected an en-US region option, got tags: %v", options)
	}
	if enUS.Get("sort") != "weight" || enUS.Get("dir") != "desc" || enUS.Get("page") != "1" {
		t.Errorf("region picker dropped sort/dir/page — these are locale-invariant and should survive a region switch: %v", enUS)
	}
}
