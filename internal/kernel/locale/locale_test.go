package locale

import (
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
