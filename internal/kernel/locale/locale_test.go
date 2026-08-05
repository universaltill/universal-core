package locale

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for name, tc := range map[string]struct {
		in       string
		wantTag  string
		wantLang string
		wantReg  string
		wantCal  Calendar
	}{
		"language only":            {"en", "en-GB", "en", "GB", CalendarGregorian},
		"language and region":      {"en-US", "en-US", "en", "US", CalendarGregorian},
		"farsi defaults to jalali": {"fa", "fa-IR", "fa", "IR", CalendarJalali},
		"arabic defaults":          {"ar", "ar-AE", "ar", "AE", CalendarGregorian},
		"turkish defaults":         {"tr", "tr-TR", "tr", "TR", CalendarGregorian},
		"explicit gregorian for farsi": {
			"fa-IR-u-ca-gregory", "fa-IR-u-ca-gregory", "fa", "IR", CalendarGregorian,
		},
		"explicit jalali for a non-default language": {
			"en-GB-u-ca-jalali", "en-GB-u-ca-jalali", "en", "GB", CalendarJalali,
		},
		"case insensitive":       {"EN-us", "en-US", "en", "US", CalendarGregorian},
		"arabic in another gulf": {"ar-QA", "ar-QA", "ar", "QA", CalendarGregorian},
	} {
		t.Run(name, func(t *testing.T) {
			l, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if l.Language != tc.wantLang || l.Region != tc.wantReg || l.Calendar != tc.wantCal {
				t.Errorf("Parse(%q) = %+v, want %s/%s/%s", tc.in, l, tc.wantLang, tc.wantReg, tc.wantCal)
			}
			// Tag round-trips: a persisted preference must reparse to
			// the same Locale, or a cookie written today reads back
			// differently tomorrow.
			if got := l.Tag(); got != tc.wantTag {
				t.Errorf("Tag() = %q, want %q", got, tc.wantTag)
			}
			back, err := Parse(l.Tag())
			if err != nil {
				t.Fatalf("re-Parse(%q): %v", l.Tag(), err)
			}
			if back.Language != l.Language || back.Region != l.Region || back.Calendar != l.Calendar {
				t.Errorf("tag did not round-trip: %+v -> %q -> %+v", l, l.Tag(), back)
			}
		})
	}
}

func TestParse_Rejections(t *testing.T) {
	for name, in := range map[string]string{
		"empty":               "",
		"blank":               "   ",
		"unsupported lang":    "de",
		"unsupported region":  "en-ZZ",
		"unsupported cal":     "en-GB-u-ca-hebrew",
		"unknown extension":   "en-GB-x-private",
		"malformed extension": "en-GB-u-ca",
	} {
		t.Run(name, func(t *testing.T) {
			if l, err := Parse(in); err == nil {
				t.Fatalf("Parse(%q) should have failed, got %+v", in, l)
			}
		})
	}
}

// Default never fails — a display formatter must always produce
// something, so an unsupported language silently becomes the fallback.
func TestDefault(t *testing.T) {
	if got := Default("fa"); got.Calendar != CalendarJalali || got.Region != "IR" {
		t.Errorf("Default(fa) = %+v", got)
	}
	if got := Default("klingon"); got.Language != FallbackLanguage || got.Region != "GB" {
		t.Errorf("Default(unsupported) = %+v, want the en-GB fallback", got)
	}
	if got := Default(""); got.Language != FallbackLanguage {
		t.Errorf("Default(empty) = %+v", got)
	}
}

func TestFormatDate(t *testing.T) {
	for name, tc := range map[string]struct {
		tag, iso, want string
	}{
		// The case the whole card exists for: same language, same date,
		// two regions, two different meanings.
		"british day first":    {"en-GB", "2026-04-03", "03/04/2026"},
		"american month first": {"en-US", "2026-04-03", "04/03/2026"},
		"turkish dots":         {"tr-TR", "2026-04-03", "03.04.2026"},
		// Arabic: day-first with Arabic-Indic digits.
		"arabic digits": {"ar-AE", "2026-04-03", "٠٣/٠٤/٢٠٢٦"},
		// Farsi: a different calendar entirely, year-first, Persian digits.
		"jalali": {"fa-IR", "2026-04-03", "۱۴۰۵/۰۱/۱۴"},
		// Nowruz: the Jalali year rolls over mid-March.
		"jalali new year eve": {"fa-IR", "2026-03-20", "۱۴۰۴/۱۲/۲۹"},
		"jalali new year day": {"fa-IR", "2026-03-21", "۱۴۰۵/۰۱/۰۱"},
		// A Farsi reader who explicitly wants Gregorian gets Gregorian,
		// still with Persian digits (script is regional, calendar isn't).
		"farsi with gregorian override": {"fa-IR-u-ca-gregory", "2026-04-03", "۲۰۲۶/۰۴/۰۳"},
	} {
		t.Run(name, func(t *testing.T) {
			l, err := Parse(tc.tag)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.tag, err)
			}
			if got := l.FormatDate(tc.iso); got != tc.want {
				t.Errorf("%s.FormatDate(%q) = %q, want %q", tc.tag, tc.iso, got, tc.want)
			}
		})
	}
}

// A value that isn't an ISO date comes back untouched: a display
// formatter must never turn unexpected stored data into an error.
func TestFormatDate_NonDatePassesThrough(t *testing.T) {
	l := Default("en")
	for _, in := range []string{"", "not a date", "2026-13-45", "2026-04-03T10:00:00Z"} {
		if got := l.FormatDate(in); got != in {
			t.Errorf("FormatDate(%q) = %q, want it returned unchanged", in, got)
		}
	}
}

func TestFormatNumber(t *testing.T) {
	for name, tc := range map[string]struct {
		tag      string
		v        float64
		decimals int
		want     string
	}{
		"british thousands":       {"en-GB", 1234567.5, 2, "1,234,567.50"},
		"turkish separators":      {"tr-TR", 1234567.5, 2, "1.234.567,50"},
		"no grouping needed":      {"en-GB", 999, 0, "999"},
		"exactly four digits":     {"en-GB", 1000, 0, "1,000"},
		"negative":                {"en-GB", -1234.5, 2, "-1,234.50"},
		"negative turkish":        {"tr-TR", -1234.5, 2, "-1.234,50"},
		"free precision":          {"en-GB", 1234.5, -1, "1,234.5"},
		"free precision integral": {"en-GB", 1234, -1, "1,234"},
		"zero":                    {"en-GB", 0, 2, "0.00"},
		"arabic digits":           {"ar-AE", 1234.5, 2, "١٬٢٣٤٫٥٠"},
		"persian digits":          {"fa-IR", 1234.5, 2, "۱٬۲۳۴٫۵۰"},
	} {
		t.Run(name, func(t *testing.T) {
			l, err := Parse(tc.tag)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.tag, err)
			}
			if got := l.FormatNumber(tc.v, tc.decimals); got != tc.want {
				t.Errorf("%s.FormatNumber(%v, %d) = %q, want %q", tc.tag, tc.v, tc.decimals, got, tc.want)
			}
		})
	}
}

func TestRTL(t *testing.T) {
	for tag, want := range map[string]bool{
		"en-GB": false, "tr-TR": false, "ar-AE": true, "fa-IR": true,
	} {
		l, err := Parse(tag)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tag, err)
		}
		if got := l.RTL(); got != want {
			t.Errorf("%s.RTL() = %v, want %v", tag, got, want)
		}
	}
}

// Every region in the rule table must be reachable and produce a
// complete rule set — a row added without separators would silently
// format numbers with no grouping at all.
func TestRegionRulesComplete(t *testing.T) {
	for region, rules := range regionRules {
		if rules.dateSep == "" || rules.decimalSep == "" || rules.groupSep == "" || rules.digits == "" {
			t.Errorf("region %s has an incomplete rule set: %+v", region, rules)
		}
		if rules.order != orderDMY && rules.order != orderMDY && rules.order != orderYMD {
			t.Errorf("region %s has an invalid date order %q", region, rules.order)
		}
	}
	// Every language default must name a region the table actually has.
	for lang, d := range languageDefaults {
		if _, ok := regionRules[d.region]; !ok {
			t.Errorf("language %s defaults to region %s, which has no rules", lang, d.region)
		}
	}
}

// The Jalali conversion, checked against known correspondences across
// leap years and month-length boundaries (month 7 onwards is 30 days,
// month 12 is 29 in a common year).
func TestGregorianToJalali(t *testing.T) {
	for _, tc := range []struct {
		gy, gm, gd int
		jy, jm, jd int
	}{
		{2026, 7, 31, 1405, 5, 9},
		{2026, 3, 21, 1405, 1, 1},   // Nowruz
		{2026, 3, 20, 1404, 12, 29}, // last day of a common Jalali year
		{2024, 3, 20, 1403, 1, 1},   // Nowruz falling on 20 March
		{2025, 3, 21, 1404, 1, 1},
		{2000, 1, 1, 1378, 10, 11},
		{1979, 2, 11, 1357, 11, 22}, // a date every Iranian calendar knows
		{2026, 9, 22, 1405, 6, 31},  // last 31-day month
		{2026, 9, 23, 1405, 7, 1},   // first 30-day month
	} {
		jy, jm, jd := gregorianToJalali(tc.gy, tc.gm, tc.gd)
		if jy != tc.jy || jm != tc.jm || jd != tc.jd {
			t.Errorf("gregorianToJalali(%d-%02d-%02d) = %d/%d/%d, want %d/%d/%d",
				tc.gy, tc.gm, tc.gd, jy, jm, jd, tc.jy, tc.jm, tc.jd)
		}
	}
}

// Walking a long run of consecutive days must produce a strictly
// well-formed Jalali sequence — days increment by one, months roll over
// exactly at their declared lengths, years at month 12's end. This is
// what catches an off-by-one in the cycle arithmetic that spot checks
// would miss.
func TestGregorianToJalali_ConsecutiveDaysAreCoherent(t *testing.T) {
	prevY, prevM, prevD := gregorianToJalali(2020, 1, 1)
	day := gregorianDayNumber(2020, 1, 1)
	for i := 1; i < 4000; i++ { // ~11 years, spanning three Jalali leap years
		y, m, d := jalaliFromDayNumber(day + i)
		switch {
		case y == prevY && m == prevM && d == prevD+1:
		case y == prevY && m == prevM+1 && d == 1:
		case y == prevY+1 && m == 1 && d == 1 && prevM == 12:
		default:
			t.Fatalf("non-consecutive Jalali step: %d/%d/%d -> %d/%d/%d", prevY, prevM, prevD, y, m, d)
		}
		if m < 1 || m > 12 || d < 1 || d > 31 {
			t.Fatalf("out-of-range Jalali date %d/%d/%d", y, m, d)
		}
		prevY, prevM, prevD = y, m, d
	}
}

// TestFormatDate_OutsideJalaliWindow is the regression test for the
// independent review's first blocker: below gregorianDayNumber's 1600
// epoch the day count went negative and Go's truncating division
// produced a NEGATIVE day-of-month — an imported 0001-01-01 "no date"
// sentinel rendered as ۰۱/-۱۱۷۳ on a Farsi list page. Outside the
// verified window the formatter now falls back to the stored ISO value,
// which is never wrong, only untranslated.
func TestFormatDate_OutsideJalaliWindow(t *testing.T) {
	fa, err := Parse("fa-IR")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, iso := range []string{
		"0001-01-01", // the classic import sentinel
		"1500-06-15",
		"1599-12-31",
		"1600-03-19",
		"1799-03-20", // one day below the verified window
		"2256-03-20", // one day above it
		"2400-01-01",
	} {
		got := fa.FormatDate(iso)
		if got != iso {
			t.Errorf("FormatDate(%q) = %q, want the ISO value unchanged (outside the verified Jalali window)", iso, got)
		}
	}
	// The window's own edges still convert.
	for _, iso := range []string{"1799-03-21", "2256-03-19", "2026-04-03"} {
		if got := fa.FormatDate(iso); got == iso {
			t.Errorf("FormatDate(%q) returned the ISO value; this date is inside the supported window", iso)
		}
	}
	// A Gregorian-calendar locale is unaffected by the Jalali window —
	// it has no conversion to get wrong.
	en := Default("en")
	if got := en.FormatDate("0001-01-01"); got != "01/01/0001" {
		t.Errorf("Gregorian FormatDate(0001-01-01) = %q, want 01/01/0001", got)
	}
}

// Indian grouping is by lakh/crore, not by thousands — offering en-IN
// in the picker while formatting it as en-GB was the review's finding.
func TestFormatNumber_IndianGrouping(t *testing.T) {
	in, err := Parse("en-IN")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for v, want := range map[float64]string{
		1234567.5:  "12,34,567.50",
		123456789:  "12,34,56,789.00",
		1000:       "1,000.00",
		999:        "999.00",
		100000:     "1,00,000.00",
		-1234567.5: "-12,34,567.50",
	} {
		if got := in.FormatNumber(v, 2); got != want {
			t.Errorf("en-IN FormatNumber(%v) = %q, want %q", v, got, want)
		}
	}
}

// A negative number on an RTL page must keep its sign to the LEFT of
// the digits — a bare "-" reorders visually and turns -1,234.50 into
// something that reads as 1,234.50-, a credit/debit ambiguity on an
// accounting list. The isolate characters are what prevent it.
func TestFormatNumber_NegativeIsolatedInRTL(t *testing.T) {
	const lri, pdi = "⁦", "⁩"
	for _, tag := range []string{"ar-AE", "fa-IR"} {
		l, err := Parse(tag)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tag, err)
		}
		got := l.FormatNumber(-1234.5, 2)
		if !strings.HasPrefix(got, lri) || !strings.HasSuffix(got, pdi) {
			t.Errorf("%s negative = %q, want it wrapped in LRI…PDI", tag, got)
		}
		if strings.Contains(strings.Trim(got, lri+pdi), lri) {
			t.Errorf("%s negative has nested isolates: %q", tag, got)
		}
		// Positives are not wrapped — no reordering to correct.
		if pos := l.FormatNumber(1234.5, 2); strings.Contains(pos, lri) {
			t.Errorf("%s positive should not be isolated, got %q", tag, pos)
		}
	}
	// LTR locales never get isolate characters.
	if got := Default("en").FormatNumber(-1234.5, 2); got != "-1,234.50" {
		t.Errorf("en negative = %q, want a plain -1,234.50", got)
	}
}

// ---- ParseNumber / ParseDate (board #74) ---------------------------------

// TestParseNumber is FormatNumber's inverse, run against FormatNumber's own
// output for a spread of locales — including the Indian lakh/crore grouping
// and Arabic/Persian digit sets, so the same cases TestFormatNumber and
// TestFormatNumber_IndianGrouping already trust are checked round-trip.
func TestParseNumber(t *testing.T) {
	for name, tc := range map[string]struct {
		tag, in, want string
	}{
		// Trailing fractional zeros are trimmed so the result matches
		// the stored value's own JSON text form regardless of how many
		// decimals the typed text carried: a FieldNumber list cell only
		// ever shows free-precision text (FormatNumber(v, -1) trims
		// trailing zeros itself), so "1,234,567.50" — carrying a
		// decimal precision the cell never actually displays — must
		// still match the same stored 1234567.5 a viewer copying the
		// cell verbatim would type.
		"british thousands":   {"en-GB", "1,234,567.50", "1234567.5"},
		"turkish separators":  {"tr-TR", "1.234.567,50", "1234567.5"},
		"no grouping needed":  {"en-GB", "999", "999"},
		"exactly four digits": {"en-GB", "1,000", "1000"},
		"negative":            {"en-GB", "-1,234.50", "-1234.5"},
		"negative turkish":    {"tr-TR", "-1.234,50", "-1234.5"},
		"free precision":      {"en-GB", "1,234.5", "1234.5"},
		// A fraction that's ALL zeros is dropped entirely, not just
		// trimmed to a bare trailing ".".
		"zero":                      {"en-GB", "0.00", "0"},
		"arabic digits":             {"ar-AE", "١٬٢٣٤٫٥٠", "1234.5"},
		"persian digits":            {"fa-IR", "۱٬۲۳۴٫۵۰", "1234.5"},
		"indian grouping":           {"en-IN", "12,34,567.50", "1234567.5"},
		"no trailing zeros to trim": {"en-GB", "1,234.56", "1234.56"},
		"plain ascii already, no separators at all": {"en-GB", "1234567.5", "1234567.5"},
		// A viewer who copy-pastes a formatted negative RTL cell brings
		// the LRI…PDI isolate characters with it — ParseNumber must
		// still recover the value.
		"arabic negative with isolates": {"ar-AE", "⁦-١٬٢٣٤٫٥٠⁩", "-1234.5"},
	} {
		t.Run(name, func(t *testing.T) {
			l, err := Parse(tc.tag)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.tag, err)
			}
			got, ok := l.ParseNumber(tc.in)
			if !ok {
				t.Fatalf("%s.ParseNumber(%q) reported not ok", tc.tag, tc.in)
			}
			if got != tc.want {
				t.Errorf("%s.ParseNumber(%q) = %q, want %q", tc.tag, tc.in, got, tc.want)
			}
		})
	}
}

// TestParseNumber_Rejections covers input that must NOT parse: garbage
// text, and — the case the whole grouping-validation exists for — digits
// that don't fit this locale's actual grouping pattern, which a naive
// "just strip the group separator" implementation would silently mangle
// instead of rejecting (tr-TR's group separator is the SAME character as
// en-GB's decimal separator, so "1234567.5" typed under tr-TR rules must
// be rejected, not silently turned into 12345675).
func TestParseNumber_Rejections(t *testing.T) {
	for name, tc := range map[string]struct{ tag, in string }{
		"empty":                                {"en-GB", ""},
		"blank":                                {"en-GB", "   "},
		"not a number":                         {"en-GB", "not a number"},
		"bare minus":                           {"en-GB", "-"},
		"en-GB decimal misread as tr grouping": {"tr-TR", "1234567.5"},
		"malformed grouping (middle group wrong size)": {"en-GB", "1,23,45"},
		"malformed grouping (leading group too long)":  {"en-GB", "12345,678"},
		"empty group":        {"en-GB", "1,,234"},
		"trailing separator": {"en-GB", "1,234,"},
		"indian grouping applied to en-GB (wrong sizes)": {"en-GB", "12,34,567"},
	} {
		t.Run(name, func(t *testing.T) {
			l, err := Parse(tc.tag)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.tag, err)
			}
			if got, ok := l.ParseNumber(tc.in); ok {
				t.Errorf("%s.ParseNumber(%q) = %q, ok, want rejected", tc.tag, tc.in, got)
			}
		})
	}
}

// TestParseNumber_RoundTrip is the property FormatNumber/ParseNumber exist
// as a pair to satisfy: format a value in a locale, parse what that same
// locale displayed, and get back the same number — across every region's
// separator/digit convention this kernel ships, including RTL's isolated
// negatives and India's non-uniform grouping.
func TestParseNumber_RoundTrip(t *testing.T) {
	tags := []string{"en-GB", "en-US", "en-IN", "tr-TR", "ar-AE", "fa-IR"}
	cases := []struct {
		v        float64
		decimals int
	}{
		{0, 2}, {999, 0}, {1000, 0}, {100000, 0},
		{1234567.5, 2}, {1234567.5, -1},
		{-1234.5, 2}, {-1, 0},
		{123456789.99, 2}, {-0.5, 2},
	}
	for _, tag := range tags {
		l, err := Parse(tag)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tag, err)
		}
		for _, tc := range cases {
			formatted := l.FormatNumber(tc.v, tc.decimals)
			norm, ok := l.ParseNumber(formatted)
			if !ok {
				t.Fatalf("%s: ParseNumber(FormatNumber(%v, %d)=%q) reported not ok",
					tag, tc.v, tc.decimals, formatted)
			}
			got, err := strconv.ParseFloat(norm, 64)
			if err != nil {
				t.Fatalf("%s: ParseNumber(%q) = %q, not a valid float: %v", tag, formatted, norm, err)
			}
			if math.Abs(got-tc.v) > 1e-6 {
				t.Errorf("%s: round trip %v -(Format,%d)-> %q -(Parse)-> %v, want %v",
					tag, tc.v, tc.decimals, formatted, got, tc.v)
			}
		}
	}
}

// TestParseDate is FormatDate's inverse, run against the exact fixtures
// TestFormatDate already trusts — including the Nowruz boundary and the
// Farsi-locale-with-Gregorian-override case.
func TestParseDate(t *testing.T) {
	for name, tc := range map[string]struct {
		tag, in, want string
	}{
		"british day first":             {"en-GB", "03/04/2026", "2026-04-03"},
		"american month first":          {"en-US", "04/03/2026", "2026-04-03"},
		"turkish dots":                  {"tr-TR", "03.04.2026", "2026-04-03"},
		"arabic digits":                 {"ar-AE", "٠٣/٠٤/٢٠٢٦", "2026-04-03"},
		"jalali":                        {"fa-IR", "۱۴۰۵/۰۱/۱۴", "2026-04-03"},
		"jalali new year eve":           {"fa-IR", "۱۴۰۴/۱۲/۲۹", "2026-03-20"},
		"jalali new year day":           {"fa-IR", "۱۴۰۵/۰۱/۰۱", "2026-03-21"},
		"farsi with gregorian override": {"fa-IR-u-ca-gregory", "۲۰۲۶/۰۴/۰۳", "2026-04-03"},
	} {
		t.Run(name, func(t *testing.T) {
			l, err := Parse(tc.tag)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.tag, err)
			}
			got, ok := l.ParseDate(tc.in)
			if !ok {
				t.Fatalf("%s.ParseDate(%q) reported not ok", tc.tag, tc.in)
			}
			if got != tc.want {
				t.Errorf("%s.ParseDate(%q) = %q, want %q", tc.tag, tc.in, got, tc.want)
			}
		})
	}
}

// TestParseDate_Rejections covers text that must NOT parse into a date:
// wrong shape, out-of-range fields, a day that gets silently normalized
// into a different date by naive arithmetic (Feb 30), and — the Jalali-
// specific case — a day that doesn't exist in a COMMON (non-leap) Jalali
// year's Esfand. 1404 is a confirmed common year (TestFormatDate's own
// "jalali new year eve" fixture: 1404/12/29 is that year's last day), so
// 1404/12/30 must be rejected, not silently accepted as some nearby date.
func TestParseDate_Rejections(t *testing.T) {
	for name, tc := range map[string]struct{ tag, in string }{
		"empty":                 {"en-GB", ""},
		"not a date":            {"en-GB", "not a date"},
		"wrong separator count": {"en-GB", "03/04"},
		"non-numeric field":     {"en-GB", "aa/04/2026"},
		"empty field":           {"en-GB", "03//2026"},
		"month out of range":    {"en-GB", "03/13/2026"},
		"day out of range":      {"en-GB", "32/04/2026"},
		"year zero":             {"en-GB", "03/04/0000"},
		// FormatDate always emits a 4-digit year — a 2-digit year isn't
		// a shape this locale's own display ever produces, so it must
		// be refused rather than silently parsed as year 26 (independent
		// review: this was previously accepted).
		"2-digit year": {"en-GB", "03/04/26"},
		// Feb 30 doesn't exist; time.Date would silently normalize it to
		// March 2 rather than reject it — the round-trip check catches
		// that instead of accepting a mistyped date as a different day.
		"nonexistent gregorian date":        {"en-GB", "30/02/2026"},
		"esfand 30 in a common jalali year": {"fa-IR", "۱۴۰۴/۱۲/۳۰"},
		// Outside FormatDate's own verified Jalali conversion window.
		"jalali year far outside the window": {"fa-IR", "۰۰۰۱/۰۱/۰۱"},
	} {
		t.Run(name, func(t *testing.T) {
			l, err := Parse(tc.tag)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.tag, err)
			}
			if got, ok := l.ParseDate(tc.in); ok {
				t.Errorf("%s.ParseDate(%q) = %q, ok, want rejected", tc.tag, tc.in, got)
			}
		})
	}
}

// TestParseDate_RoundTrip: FormatDate then ParseDate must return the
// original ISO date, for every day across a multi-year span (crossing
// several Nowruz boundaries and both common and leap Jalali years) and
// every locale/calendar this kernel ships.
func TestParseDate_RoundTrip(t *testing.T) {
	tags := []string{"en-GB", "en-US", "tr-TR", "ar-AE", "fa-IR"}
	for _, tag := range tags {
		l, err := Parse(tag)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tag, err)
		}
		start := gregorianDayNumber(2018, 1, 1)
		for i := 0; i < 3000; i += 11 { // sampled, not exhaustive — keeps the test fast
			gy, gm, gd := gregorianDateFromDayNumber(start + i)
			iso := isoDate(gy, gm, gd)
			formatted := l.FormatDate(iso)
			got, ok := l.ParseDate(formatted)
			if !ok {
				t.Fatalf("%s: ParseDate(FormatDate(%s)=%q) reported not ok", tag, iso, formatted)
			}
			if got != iso {
				t.Fatalf("%s: round trip %s -(Format)-> %q -(Parse)-> %s", tag, iso, formatted, got)
			}
		}
	}
}

// TestParseDate_JalaliLeapEsfand30 closes a gap the independent review
// found in TestParseDate_RoundTrip: that test's sampling stride (every
// 11th day over 2018-2020) never happens to land on a leap Jalali
// year's Esfand 30th — the one day FormatDate/ParseDate's Jalali path
// has to get right that TestParseDate_Rejections' "esfand 30 in a
// COMMON year must be refused" case doesn't cover from the other side.
// Rather than hardcode a guessed ISO date, this SEARCHES the supported
// window for a real jm==12, jd==30 day (gregorianToJalali is already
// verified elsewhere) and round-trips that.
func TestParseDate_JalaliLeapEsfand30(t *testing.T) {
	fa, err := Parse("fa-IR")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	lo := gregorianDayNumber(jalaliMinGregorian[0], jalaliMinGregorian[1], jalaliMinGregorian[2])
	hi := gregorianDayNumber(jalaliMaxGregorian[0], jalaliMaxGregorian[1], jalaliMaxGregorian[2])
	found := 0
	for n := lo; n <= hi && found < 5; n++ {
		gy, gm, gd := gregorianDateFromDayNumber(n)
		jy, jm, jd := gregorianToJalali(gy, gm, gd)
		if jm != 12 || jd != 30 {
			continue
		}
		found++
		iso := isoDate(gy, gm, gd)
		formatted := fa.FormatDate(iso)
		got, ok := fa.ParseDate(formatted)
		if !ok {
			t.Fatalf("leap Esfand 30 (jalali %d/12/30, gregorian %s): ParseDate(%q) reported not ok", jy, iso, formatted)
		}
		if got != iso {
			t.Errorf("leap Esfand 30 (jalali %d/12/30): round trip %s -(Format)-> %q -(Parse)-> %s", jy, iso, formatted, got)
		}
	}
	if found == 0 {
		t.Fatal("found no leap Jalali Esfand-30 day in the supported window — the search itself is broken")
	}
}

func isoDate(y, m, d int) string {
	return strings.Join([]string{
		strconv.Itoa(y),
		fmt2(m),
		fmt2(d),
	}, "-")
}

func fmt2(v int) string {
	s := strconv.Itoa(v)
	if len(s) < 2 {
		return "0" + s
	}
	return s
}

// TestDelocalizeDigits directly checks the ASCII round trip
// localizeDigits/delocalizeDigits form: mapping a locale's own digit
// shapes back to ASCII, and passing already-ASCII text (or a Latin-digit
// locale) through unchanged.
func TestDelocalizeDigits(t *testing.T) {
	fa, err := Parse("fa-IR")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := fa.delocalizeDigits("۱۲۳/۴۵۶"); got != "123/456" {
		t.Errorf("delocalizeDigits(persian) = %q, want 123/456", got)
	}
	en := Default("en")
	if got := en.delocalizeDigits("123/456"); got != "123/456" {
		t.Errorf("delocalizeDigits(already ascii) = %q, want unchanged", got)
	}
}

// TestJalaliToGregorian_KnownCorrespondences cross-checks jalaliToGregorian
// against the exact fixtures TestGregorianToJalali already trusts, in the
// reverse direction.
func TestJalaliToGregorian_KnownCorrespondences(t *testing.T) {
	for _, tc := range []struct {
		gy, gm, gd int
		jy, jm, jd int
	}{
		{2026, 7, 31, 1405, 5, 9},
		{2026, 3, 21, 1405, 1, 1},
		{2026, 3, 20, 1404, 12, 29},
		{2024, 3, 20, 1403, 1, 1},
		{2025, 3, 21, 1404, 1, 1},
		{2000, 1, 1, 1378, 10, 11},
		{1979, 2, 11, 1357, 11, 22},
		{2026, 9, 22, 1405, 6, 31},
		{2026, 9, 23, 1405, 7, 1},
	} {
		gy, gm, gd, ok := jalaliToGregorian(tc.jy, tc.jm, tc.jd)
		if !ok {
			t.Fatalf("jalaliToGregorian(%d/%d/%d) reported not ok", tc.jy, tc.jm, tc.jd)
		}
		if gy != tc.gy || gm != tc.gm || gd != tc.gd {
			t.Errorf("jalaliToGregorian(%d/%d/%d) = %04d-%02d-%02d, want %04d-%02d-%02d",
				tc.jy, tc.jm, tc.jd, gy, gm, gd, tc.gy, tc.gm, tc.gd)
		}
	}
}

// TestJalaliToGregorian_RoundTripsWithGregorianToJalali sweeps a
// multi-year run of consecutive Gregorian days (crossing several Jalali
// leap-cycle boundaries, so both common and leap Esfand lengths are
// exercised) and confirms gregorianToJalali then jalaliToGregorian
// returns the exact original date — the property the binary-search
// implementation exists to guarantee, rather than a hand-derived inverse
// formula that could disagree with the forward one in a new, independent
// way.
func TestJalaliToGregorian_RoundTripsWithGregorianToJalali(t *testing.T) {
	start := gregorianDayNumber(2015, 1, 1)
	for i := 0; i < 6000; i++ {
		gy, gm, gd := gregorianDateFromDayNumber(start + i)
		jy, jm, jd := gregorianToJalali(gy, gm, gd)
		backY, backM, backD, ok := jalaliToGregorian(jy, jm, jd)
		if !ok {
			t.Fatalf("jalaliToGregorian(%d/%d/%d) (from %04d-%02d-%02d) reported not ok",
				jy, jm, jd, gy, gm, gd)
		}
		if backY != gy || backM != gm || backD != gd {
			t.Fatalf("round trip mismatch: %04d-%02d-%02d -> jalali %d/%d/%d -> %04d-%02d-%02d",
				gy, gm, gd, jy, jm, jd, backY, backM, backD)
		}
	}
}

// TestJalaliToGregorian_Rejections covers input jalaliToGregorian must
// refuse: out-of-range month/day (rejected before the search even runs)
// and a year far outside the window FormatDate itself supports (rejected
// by the search finding no match).
func TestJalaliToGregorian_Rejections(t *testing.T) {
	for name, tc := range map[string]struct{ jy, jm, jd int }{
		"zero month":            {1405, 0, 1},
		"month 13":              {1405, 13, 1},
		"zero day":              {1405, 1, 0},
		"day 32":                {1405, 1, 32},
		"year far below window": {1, 1, 1},
		"year far above window": {9999, 1, 1},
	} {
		t.Run(name, func(t *testing.T) {
			if gy, gm, gd, ok := jalaliToGregorian(tc.jy, tc.jm, tc.jd); ok {
				t.Errorf("jalaliToGregorian(%d,%d,%d) = %d/%d/%d, ok, want rejected",
					tc.jy, tc.jm, tc.jd, gy, gm, gd)
			}
		})
	}
}
