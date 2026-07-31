// Package locale is this kernel's regional-formatting engine: how a
// date and a number are WRITTEN for a viewer, as opposed to which
// language the surrounding words are in (internal/i18n's catalog, and
// internal/api's language switcher, already own that).
//
// The distinction is the whole point of this package (board #22). A
// visitor reading English in London and one reading English in New York
// share every translated string and disagree about what 03/04/2026
// means; a Farsi reader in Tehran expects 1405/01/15 rather than
// 2026-04-04. Language alone cannot express either, so a Locale is
// language + region + calendar.
//
// Scope boundary — DISPLAY ONLY. Everything this package formats is
// read-only output (list cells, report pages). Stored values stay ISO
// 8601 / plain decimal, and every form INPUT keeps rendering the raw
// stored value, because those round-trip back through
// csvimport.Coerce on submit and a localized string would either fail
// to parse or, worse, parse as the wrong date. Localized date INPUT
// (pickers, parsing "03/04/2026" by region) is deliberately a separate
// piece of work — see the package's follow-up card.
//
// No new dependency: golang.org/x/text is already in the module graph
// but its formatting side assumes CLDR data this kernel doesn't vendor
// and offers no non-Gregorian calendar at all, which is precisely what
// the Farsi locale needs. The rule table below is small, explicit, and
// reviewable — the same "a short honest table beats a partial CLDR"
// choice internal/i18n itself made.
package locale

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Calendar names a calendar system a locale can render dates in.
type Calendar string

const (
	// CalendarGregorian is the default for every locale except Farsi.
	CalendarGregorian Calendar = "gregorian"
	// CalendarJalali is the Solar Hijri calendar — the civil calendar of
	// Iran and Afghanistan, and what a Farsi-speaking user means by a
	// date. Not a variant of Gregorian: the year, month and day are all
	// different numbers, so this is a conversion, not a format string.
	CalendarJalali Calendar = "jalali"
)

// DigitSet names the digit shapes a locale renders numerals in.
type DigitSet string

const (
	DigitsLatin DigitSet = "latn"
	// DigitsArabic is Arabic-Indic (٠١٢…), conventional in Arabic-script
	// locales. Applied to formatted OUTPUT only — never to stored data,
	// which stays ASCII.
	DigitsArabic DigitSet = "arab"
	// DigitsPersian is Extended Arabic-Indic (۰۱۲…) — visually distinct
	// from DigitsArabic for four digits and the correct set for Farsi.
	DigitsPersian DigitSet = "arabext"
)

// dateOrder is the field order a region writes a date in.
type dateOrder string

const (
	orderDMY dateOrder = "dmy"
	orderMDY dateOrder = "mdy"
	orderYMD dateOrder = "ymd"
)

// Locale is a resolved regional-formatting preference: which language's
// words, which region's conventions, which calendar's dates. The zero
// value is not useful — build one with Parse or Default.
type Locale struct {
	// Language is the base language subtag ("en", "ar", "tr", "fa") —
	// the same value internal/i18n's catalog is keyed by, so a Locale
	// can drive both concerns without a second lookup.
	Language string
	// Region is the ISO 3166-1 alpha-2 region subtag ("GB", "US", "AE"),
	// uppercase. Never empty on a Parse/Default result: an unqualified
	// language resolves to its default region below.
	Region string
	// Calendar is the calendar system dates render in.
	Calendar Calendar

	rules formatRules
}

// formatRules is everything region-dependent about writing a date or a
// number. Held as data so adding a region is a table row, not code.
type formatRules struct {
	order      dateOrder
	dateSep    string
	groupSep   string
	decimalSep string
	digits     DigitSet
	// groupSizes is the digit-grouping pattern, least-significant group
	// first. {3} is the near-universal thousands grouping; {3, 2} is the
	// Indian lakh/crore convention (12,34,567) — the last size repeats
	// for the remaining leading digits. Zero value means {3}.
	groupSizes []int
}

// regionRules maps a region to its conventions. Deliberately partial:
// only regions this product actually targets (the launch markets and
// the ones its locales imply) are listed, and anything unlisted falls
// back to the language default rather than pretending to know. Adding a
// market is one line here plus a test row.
var regionRules = map[string]formatRules{
	// Latin-script, day-first — the majority convention.
	"GB": {order: orderDMY, dateSep: "/", groupSep: ",", decimalSep: ".", digits: DigitsLatin},
	"IE": {order: orderDMY, dateSep: "/", groupSep: ",", decimalSep: ".", digits: DigitsLatin},
	"AU": {order: orderDMY, dateSep: "/", groupSep: ",", decimalSep: ".", digits: DigitsLatin},
	"NZ": {order: orderDMY, dateSep: "/", groupSep: ",", decimalSep: ".", digits: DigitsLatin},
	// India groups by lakh/crore (12,34,567), not by thousands.
	"IN": {order: orderDMY, dateSep: "/", groupSep: ",", decimalSep: ".", digits: DigitsLatin, groupSizes: []int{3, 2}},
	// Month-first — effectively only the US.
	"US": {order: orderMDY, dateSep: "/", groupSep: ",", decimalSep: ".", digits: DigitsLatin},
	// Turkey: day-first with dots, and the European separator pair.
	"TR": {order: orderDMY, dateSep: ".", groupSep: ".", decimalSep: ",", digits: DigitsLatin},
	// Gulf/Levant Arabic markets: day-first, Arabic-Indic digits, and
	// the Arabic separators (U+066C thousands, U+066B decimal) — an
	// ASCII comma next to Arabic-Indic digits reads as a mismatch, and
	// these are the separators the digits are designed to pair with.
	"AE": {order: orderDMY, dateSep: "/", groupSep: "٬", decimalSep: "٫", digits: DigitsArabic},
	"SA": {order: orderDMY, dateSep: "/", groupSep: "٬", decimalSep: "٫", digits: DigitsArabic},
	"QA": {order: orderDMY, dateSep: "/", groupSep: "٬", decimalSep: "٫", digits: DigitsArabic},
	"KW": {order: orderDMY, dateSep: "/", groupSep: "٬", decimalSep: "٫", digits: DigitsArabic},
	"BH": {order: orderDMY, dateSep: "/", groupSep: "٬", decimalSep: "٫", digits: DigitsArabic},
	"OM": {order: orderDMY, dateSep: "/", groupSep: "٬", decimalSep: "٫", digits: DigitsArabic},
	"EG": {order: orderDMY, dateSep: "/", groupSep: "٬", decimalSep: "٫", digits: DigitsArabic},
	// Iran/Afghanistan: Jalali dates are written year-first with
	// slashes, same Arabic separators with the Persian digit shapes.
	"IR": {order: orderYMD, dateSep: "/", groupSep: "٬", decimalSep: "٫", digits: DigitsPersian},
	"AF": {order: orderYMD, dateSep: "/", groupSep: "٬", decimalSep: "٫", digits: DigitsPersian},
}

// languageDefaults gives every supported language a default region and
// calendar, so a language-only preference (which is all the existing
// cookie carries) still produces a complete, sensible Locale.
var languageDefaults = map[string]struct {
	region   string
	calendar Calendar
}{
	"en": {"GB", CalendarGregorian},
	"ar": {"AE", CalendarGregorian},
	"tr": {"TR", CalendarGregorian},
	"fa": {"IR", CalendarJalali},
}

// FallbackLanguage is the language used when a preference names none
// this kernel supports — the same "en" internal/i18n falls back to.
const FallbackLanguage = "en"

// Default returns the Locale for a bare language subtag, filling in
// that language's conventional region and calendar. An unsupported
// language falls back to FallbackLanguage rather than erroring: a
// display formatter must always produce something.
func Default(language string) Locale {
	lang := strings.ToLower(strings.TrimSpace(language))
	d, ok := languageDefaults[lang]
	if !ok {
		lang = FallbackLanguage
		d = languageDefaults[FallbackLanguage]
	}
	return build(lang, d.region, d.calendar)
}

// Parse resolves a preference string into a Locale. Accepted forms:
//
//	"en"                  language only — conventional region/calendar
//	"en-GB"               language + region
//	"fa-IR-u-ca-jalali"   explicit calendar, BCP 47 -u-ca- extension
//	"en-US-u-ca-gregory"  ("gregory" is BCP 47's spelling of gregorian)
//
// Unknown region or calendar is an error rather than a silent
// substitution — a stored preference that no longer means anything
// should surface, not quietly render American dates to a British user.
// Callers reading an untrusted value (a cookie, a query parameter)
// should treat an error as "use Default" — see internal/api's own
// resolution.
func Parse(s string) (Locale, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Locale{}, fmt.Errorf("locale: empty preference")
	}
	parts := strings.Split(raw, "-")
	lang := strings.ToLower(parts[0])
	d, ok := languageDefaults[lang]
	if !ok {
		return Locale{}, fmt.Errorf("locale: unsupported language %q", parts[0])
	}
	region, calendar := d.region, d.calendar

	rest := parts[1:]
	if len(rest) > 0 && !strings.EqualFold(rest[0], "u") {
		region = strings.ToUpper(rest[0])
		if _, ok := regionRules[region]; !ok {
			return Locale{}, fmt.Errorf("locale: unsupported region %q", rest[0])
		}
		rest = rest[1:]
	}
	if len(rest) > 0 {
		// The only BCP 47 extension this kernel reads is -u-ca-<cal>.
		if len(rest) != 3 || !strings.EqualFold(rest[0], "u") || !strings.EqualFold(rest[1], "ca") {
			return Locale{}, fmt.Errorf("locale: unsupported extension in %q (only -u-ca- is understood)", raw)
		}
		switch strings.ToLower(rest[2]) {
		case "gregory", string(CalendarGregorian):
			calendar = CalendarGregorian
		case string(CalendarJalali), "persian":
			calendar = CalendarJalali
		default:
			return Locale{}, fmt.Errorf("locale: unsupported calendar %q", rest[2])
		}
	}
	return build(lang, region, calendar), nil
}

func build(lang, region string, calendar Calendar) Locale {
	rules, ok := regionRules[region]
	if !ok {
		rules = regionRules[languageDefaults[FallbackLanguage].region]
	}
	return Locale{Language: lang, Region: region, Calendar: calendar, rules: rules}
}

// Tag renders the Locale back into the same string form Parse accepts —
// the value persisted in the preference cookie. The calendar extension
// is emitted only when it differs from the language's default, keeping
// the common case short ("en-GB", not "en-GB-u-ca-gregory").
func (l Locale) Tag() string {
	tag := l.Language + "-" + l.Region
	if d, ok := languageDefaults[l.Language]; ok && d.calendar == l.Calendar {
		return tag
	}
	cal := string(l.Calendar)
	if l.Calendar == CalendarGregorian {
		cal = "gregory"
	}
	return tag + "-u-ca-" + cal
}

// FormatDate writes an ISO-8601 date (the storage form of every
// entity.FieldDate value) the way this locale's region and calendar
// write it. A value that is not an ISO date is returned unchanged: a
// display formatter must never turn unexpected data into an error page,
// and showing the raw stored text is the honest fallback.
func (l Locale) FormatDate(iso string) string {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(iso))
	if err != nil {
		return iso
	}
	y, m, d := t.Year(), int(t.Month()), t.Day()
	if l.Calendar == CalendarJalali {
		if !jalaliSupported(y, m, d) {
			// Outside the conversion's verified window the arithmetic
			// would either underflow (a negative day-of-month — an
			// imported 0001-01-01 "no date" sentinel rendered as
			// -۶۱۷/۰۱/-۱۱۷۳) or silently disagree with the Iranian
			// civil calendar. Showing the stored ISO value is the same
			// honest fallback an unparseable value already gets, and it
			// is never wrong, only untranslated. Independent review
			// found both edges.
			return iso
		}
		y, m, d = gregorianToJalali(y, m, d)
	}

	yy := fmt.Sprintf("%04d", y)
	mm := fmt.Sprintf("%02d", m)
	dd := fmt.Sprintf("%02d", d)
	var out string
	switch l.rules.order {
	case orderMDY:
		out = mm + l.rules.dateSep + dd + l.rules.dateSep + yy
	case orderYMD:
		out = yy + l.rules.dateSep + mm + l.rules.dateSep + dd
	default: // orderDMY
		out = dd + l.rules.dateSep + mm + l.rules.dateSep + yy
	}
	return l.localizeDigits(out)
}

// FormatNumber writes a number with this locale's grouping and decimal
// separators. decimals < 0 keeps the value's own precision (trailing
// zeros trimmed), matching the existing FormatFieldValue behaviour for
// unqualified numbers; decimals >= 0 fixes the scale, which is what a
// money column wants (its Currency's minor_unit).
func (l Locale) FormatNumber(v float64, decimals int) string {
	prec := decimals
	if prec < 0 {
		prec = -1
	}
	s := strconv.FormatFloat(v, 'f', prec, 64)

	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, frac, hasFrac := strings.Cut(s, ".")

	out := groupDigits(intPart, l.rules.groupSep, l.rules.groupSizes)
	if hasFrac {
		out += l.rules.decimalSep + frac
	}
	if neg {
		out = "-" + out
		if l.RTL() {
			// A bare leading "-" inside a dir="rtl" page renders to the
			// RIGHT of the digits, so -1,234.50 reads as 1,234.50- —
			// a credit/debit ambiguity on an accounting list page
			// (measured in a real browser by the independent review).
			// LRI…PDI isolates the number so its sign stays where it
			// belongs without affecting the surrounding text.
			out = "⁦" + out + "⁩"
		}
	}
	return l.localizeDigits(out)
}

// RTL reports whether this locale's script runs right to left — kept
// here so direction travels with the Locale rather than being looked up
// from a second table beside it.
func (l Locale) RTL() bool {
	return l.Language == "ar" || l.Language == "fa"
}

// groupDigits inserts sep according to the locale's grouping pattern,
// working from the least-significant end so a non-uniform pattern (the
// Indian {3, 2}: 1,23,45,678) comes out right. The last size in the
// pattern repeats for every remaining leading group.
func groupDigits(digits, sep string, sizes []int) string {
	if len(sizes) == 0 {
		sizes = []int{3}
	}
	if sep == "" || len(digits) <= sizes[0] {
		return digits
	}
	var groups []string
	rest := digits
	for i := 0; len(rest) > 0; i++ {
		size := sizes[len(sizes)-1] // the last size repeats
		if i < len(sizes) {
			size = sizes[i]
		}
		if len(rest) <= size {
			groups = append([]string{rest}, groups...)
			break
		}
		groups = append([]string{rest[len(rest)-size:]}, groups...)
		rest = rest[:len(rest)-size]
	}
	return strings.Join(groups, sep)
}

var digitRunes = map[DigitSet][]rune{
	DigitsArabic:  []rune("٠١٢٣٤٥٦٧٨٩"),
	DigitsPersian: []rune("۰۱۲۳۴۵۶۷۸۹"),
}

// localizeDigits maps ASCII digits to the locale's digit set. Only
// digits are touched — separators stay as the rule table defines them.
func (l Locale) localizeDigits(s string) string {
	set, ok := digitRunes[l.rules.digits]
	if !ok {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(set[r-'0'])
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---- Jalali (Solar Hijri) conversion ------------------------------------
// The classic day-count conversion: count days from a fixed Gregorian
// epoch, shift by the 79-day offset between the two calendars' year
// starts, then walk the Jalali 33-year leap cycle (12053 days) and its
// month lengths (six 31s, five 30s, then 29 or 30). Pure integer
// arithmetic — no table, no locale data, no dependency — and exact
// across the whole range this product can plausibly store. The
// leap-year rule it encodes is the arithmetic 33-year approximation,
// which agrees with the observational Iranian civil calendar for all
// contemporary dates.

// jalaliMonthDays is the length of each Jalali month in a common year;
// Esfand (the 12th) gains a day in a leap year, handled by the
// day-count arithmetic rather than by mutating this table.
var jalaliMonthDays = [12]int{31, 31, 31, 31, 31, 31, 30, 30, 30, 30, 30, 29}

var gregorianMonthDays = [12]int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

func isGregorianLeap(y int) bool {
	return (y%4 == 0 && y%100 != 0) || y%400 == 0
}

// gregorianDayNumber counts days from the start of 1600-01-01 (day 0).
func gregorianDayNumber(gy, gm, gd int) int {
	y := gy - 1600
	n := 365*y + (y+3)/4 - (y+99)/100 + (y+399)/400
	for i := 0; i < gm-1; i++ {
		n += gregorianMonthDays[i]
	}
	if gm > 2 && isGregorianLeap(gy) {
		n++
	}
	return n + gd - 1
}

// jalaliMinGregorian / jalaliMaxGregorian bound the window in which
// this arithmetic agrees with the Iranian civil calendar. Below the
// lower bound the day count underflows outright (gregorianDayNumber's
// epoch is 1600-01-01); beyond either bound the 33-year approximation
// drifts from the observational calendar around Nowruz in scattered
// years — the independent review swept 1600-2400 day-by-day against a
// structurally different implementation and found the exact edges
// quoted here. FormatDate refuses rather than guessing outside them.
var (
	jalaliMinGregorian = [3]int{1799, 3, 21}
	jalaliMaxGregorian = [3]int{2256, 3, 19}
)

func jalaliSupported(y, m, d int) bool {
	v := [3]int{y, m, d}
	return !lessThan(v, jalaliMinGregorian) && !lessThan(jalaliMaxGregorian, v)
}

func lessThan(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// gregorianToJalali converts a Gregorian Y/M/D to the Solar Hijri
// calendar, for dates inside the window jalaliSupported bounds.
// Verified in the tests against known correspondences, including the
// Nowruz boundary where the Jalali year rolls over in mid-March and the
// leap-year case where it falls on 20 March.
func gregorianToJalali(gy, gm, gd int) (jy, jm, jd int) {
	return jalaliFromDayNumber(gregorianDayNumber(gy, gm, gd))
}

// jalaliFromDayNumber is the conversion proper, taking the same
// 1600-01-01-based day count gregorianDayNumber produces. Split out so
// the tests can walk consecutive days without re-deriving a Gregorian
// date for each one.
func jalaliFromDayNumber(gregorianDays int) (jy, jm, jd int) {
	dayNo := gregorianDays - 79

	cycles := dayNo / 12053 // whole 33-year cycles
	dayNo %= 12053
	jy = 979 + 33*cycles + 4*(dayNo/1461)
	dayNo %= 1461
	if dayNo >= 366 {
		jy += (dayNo - 1) / 365
		dayNo = (dayNo - 1) % 365
	}

	i := 0
	for ; i < 11 && dayNo >= jalaliMonthDays[i]; i++ {
		dayNo -= jalaliMonthDays[i]
	}
	return jy, i + 1, dayNo + 1
}
