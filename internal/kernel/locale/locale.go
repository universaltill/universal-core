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

// lri/pdi are the bidi isolate characters FormatNumber wraps around a
// negative RTL amount (see the isolate comment below) and stripIsolates
// removes on the way back in — one pair of literals shared by both
// directions instead of two copies that could drift apart.
const lri, pdi = "⁦", "⁩"

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
			out = lri + out + pdi
		}
	}
	return l.localizeDigits(out)
}

// ParseNumber is the inverse of FormatNumber: given text typed in this
// locale's own digits/separators, it returns the plain ASCII decimal
// string (no grouping, "." for the decimal point) that the stored
// value's own JSON text representation compares against. This exists
// for the list-filter box (board #74): since #22 a cell shows
// "1,234,567.5" or "۱٬۲۳۴٬۵۶۷٫۵", but the stored/filtered value is
// always plain ASCII, so a viewer who copies what the page shows them
// into the filter and gets zero rows back is hitting exactly the gap
// this closes.
//
// ok is false for text that isn't a valid number under this locale's
// rules (wrong separators, stray characters, empty) — display-only
// boundary, same as FormatDate/FormatNumber never erroring: the caller
// falls back to matching the raw typed text rather than rejecting the
// filter outright.
func (l Locale) ParseNumber(s string) (string, bool) {
	s = l.delocalizeDigits(stripIsolates(strings.TrimSpace(s)))
	if s == "" {
		return "", false
	}
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	intPart, fracPart, hasFrac := s, "", false
	if l.rules.decimalSep != "" {
		if i, f, ok := strings.Cut(s, l.rules.decimalSep); ok {
			intPart, fracPart, hasFrac = i, f, true
		}
	}
	// Validate the grouping BEFORE stripping it: en-GB's decimal "." and
	// tr-TR's group "." are the same character, so "1234567.5" typed
	// under tr-TR rules would otherwise strip a false decimal point into
	// a nonsense 8-digit run instead of being recognized as not a valid
	// Turkish grouping. An ungrouped run (no separator present at all)
	// is always accepted — that is the plain-ASCII case #74 itself is
	// built around.
	if !validGrouping(intPart, l.rules.groupSep, l.rules.groupSizes) {
		return "", false
	}
	if l.rules.groupSep != "" {
		intPart = strings.ReplaceAll(intPart, l.rules.groupSep, "")
	}
	if intPart == "" || !isASCIIDigits(intPart) || (hasFrac && !isASCIIDigits(fracPart)) {
		return "", false
	}
	// Trailing fractional zeros are trimmed (and a fraction that's now
	// all zeros is dropped entirely) so the result matches the stored
	// value's OWN JSON text form regardless of how many decimals the
	// typed text carried — a cell showing "1,234,567.5" (FormatNumber's
	// free-precision -1) and a viewer who instead types "1,234,567.50"
	// must both match the same stored 1234567.5 (independent review:
	// without this, the fixed-precision form silently found nothing).
	if hasFrac {
		fracPart = strings.TrimRight(fracPart, "0")
	}
	out := intPart
	if fracPart != "" {
		out += "." + fracPart
	}
	if neg {
		out = "-" + out
	}
	return out, true
}

// ParseDate is the inverse of FormatDate: given text typed in this
// locale's script, field order and calendar, it returns the ISO-8601
// form (`entity.FieldDate`'s storage form) the same way FormatDate
// produces the display form from it. Same board #74 motivation as
// ParseNumber, for the date half of the filter box.
//
// ok is false for text that doesn't parse as a real date under this
// locale — an out-of-range day/month, a Jalali date outside the window
// FormatDate itself refuses (jalaliSupported), or anything that isn't
// three locale-digit groups separated by this region's date separator.
// The caller falls back to matching the raw typed text, never an error
// page — the same display-only-boundary contract FormatDate keeps.
func (l Locale) ParseDate(s string) (string, bool) {
	// FormatDate never wraps a date in LRI/PDI isolates (only
	// FormatNumber's negative-RTL case does) — stripping them here
	// anyway costs nothing and keeps ParseDate/ParseNumber symmetric
	// rather than leaving a footgun for whichever one of them changes
	// first.
	s = l.delocalizeDigits(stripIsolates(strings.TrimSpace(s)))
	if l.rules.dateSep == "" {
		return "", false
	}
	parts := strings.Split(s, l.rules.dateSep)
	if len(parts) != 3 {
		return "", false
	}
	for _, p := range parts {
		if !isASCIIDigits(p) {
			return "", false
		}
	}
	var y, m, d int
	var yearPart string
	switch l.rules.order {
	case orderMDY:
		m, _ = strconv.Atoi(parts[0])
		d, _ = strconv.Atoi(parts[1])
		yearPart = parts[2]
	case orderYMD:
		yearPart = parts[0]
		m, _ = strconv.Atoi(parts[1])
		d, _ = strconv.Atoi(parts[2])
	default: // orderDMY
		d, _ = strconv.Atoi(parts[0])
		m, _ = strconv.Atoi(parts[1])
		yearPart = parts[2]
	}
	// FormatDate always emits a 4-digit year (%04d) — anything else
	// isn't a shape this locale's own display ever produces. Without
	// this, "03/04/26" silently parses as the year 26 instead of being
	// refused (independent review): not a false-match risk (no stored
	// FieldDate is ever year 26), but it defeats the honest raw-text
	// fallback for input that plainly isn't a real regional date.
	if len(yearPart) != 4 {
		return "", false
	}
	y, _ = strconv.Atoi(yearPart)
	if y < 1 || m < 1 || m > 12 || d < 1 {
		return "", false
	}
	if l.Calendar == CalendarJalali {
		gy, gm, gd, ok := jalaliToGregorian(y, m, d)
		if !ok {
			return "", false
		}
		y, m, d = gy, gm, gd
	}
	// time.Date silently normalizes an out-of-range day (Feb 30 rolls
	// into March) rather than erroring — checking the round trip
	// against what was asked for is what catches that instead of
	// silently accepting a mistyped date as a different, nearby one.
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	if t.Year() != y || int(t.Month()) != m || t.Day() != d {
		return "", false
	}
	return t.Format("2006-01-02"), true
}

// stripIsolates removes the LRI/PDI bidi-isolate characters FormatNumber
// wraps around a negative RTL amount (see the isolate comment above) —
// present if a viewer copy-pasted a formatted cell's text verbatim
// rather than retyping it.
func stripIsolates(s string) string {
	s = strings.ReplaceAll(s, lri, "")
	return strings.ReplaceAll(s, pdi, "")
}

// validGrouping reports whether intPart's digit groups (split on sep)
// match the sizes a real FormatNumber output in this locale would have
// produced — the exact inverse check for groupDigits, read right to
// left: the rightmost group is sizes[0], each group in further reads
// sizes[1], sizes[2]…, and the last size repeats for any remaining
// leading groups, same as groupDigits' own iteration. The leftmost
// group is allowed to be SHORTER than its expected size (a 4-digit
// number like "1,234" has a 1-digit leading group), every other group
// must match exactly. No separator present at all (a single segment)
// is always valid — that's plain ASCII input, not a grouping claim to
// validate.
func validGrouping(intPart, sep string, sizes []int) bool {
	if sep == "" {
		return true
	}
	segs := strings.Split(intPart, sep)
	if len(segs) == 1 {
		return true
	}
	if len(sizes) == 0 {
		sizes = []int{3}
	}
	n := len(segs)
	for i, seg := range segs {
		if seg == "" {
			return false
		}
		groupIdx := n - 1 - i // 0 = rightmost/least-significant group
		size := sizes[len(sizes)-1]
		if groupIdx < len(sizes) {
			size = sizes[groupIdx]
		}
		if i == 0 {
			// The empty-segment check above already rules out len(seg)
			// < 1 — only the upper bound is a real constraint here.
			if len(seg) > size {
				return false
			}
		} else if len(seg) != size {
			return false
		}
	}
	return true
}

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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

// delocalizeDigits is localizeDigits' inverse: maps a locale's own
// digit shapes back to ASCII. A locale with no non-Latin digit set
// (digitRunes has no entry for it) is returned unchanged — its digits
// are already ASCII, same identity behaviour localizeDigits itself has
// for DigitsLatin.
func (l Locale) delocalizeDigits(s string) string {
	set, ok := digitRunes[l.rules.digits]
	if !ok {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		mapped := r
		for i, dr := range set {
			if dr == r {
				mapped = rune('0' + i)
				break
			}
		}
		b.WriteRune(mapped)
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

// jalaliToGregorian is the inverse of gregorianToJalali: given a Solar
// Hijri date, it returns the Gregorian date whose day count maps to it
// (board #74, ParseDate). Implemented as a binary search over
// gregorianDayNumber rather than a second, independently re-derived
// cycle-arithmetic formula — the forward conversion is already the
// tested source of truth for the 33-year-cycle math
// (TestGregorianToJalali*), and the day-count basis both conversions
// share is strictly monotonic
// (TestGregorianToJalali_ConsecutiveDaysAreCoherent), so searching it is
// exact, not approximate, and carries none of the risk of a second,
// independently-wrong sign or off-by-one in the same arithmetic that
// hand-deriving the inverse cycle math a second way would.
//
// ok is false for a (jy, jm, jd) with no Gregorian equivalent inside the
// window FormatDate itself supports (jalaliSupported's bounds) —
// including a day that doesn't exist in that Jalali month (Esfand 30 in
// a common, non-leap Jalali year).
func jalaliToGregorian(jy, jm, jd int) (gy, gm, gd int, ok bool) {
	if jm < 1 || jm > 12 || jd < 1 || jd > 31 {
		return 0, 0, 0, false
	}
	lo := gregorianDayNumber(jalaliMinGregorian[0], jalaliMinGregorian[1], jalaliMinGregorian[2])
	hi := gregorianDayNumber(jalaliMaxGregorian[0], jalaliMaxGregorian[1], jalaliMaxGregorian[2])
	target := [3]int{jy, jm, jd}
	for lo <= hi {
		mid := lo + (hi-lo)/2
		y, m, d := jalaliFromDayNumber(mid)
		got := [3]int{y, m, d}
		switch {
		case got == target:
			gy, gm, gd = gregorianDateFromDayNumber(mid)
			return gy, gm, gd, true
		case lessThan(got, target):
			lo = mid + 1
		default:
			hi = mid - 1
		}
	}
	return 0, 0, 0, false
}

// gregorianDateFromDayNumber is gregorianDayNumber's inverse: the
// Gregorian calendar date n days after the 1600-01-01 epoch. Delegates
// to time.Time's own proleptic-Gregorian arithmetic rather than
// hand-rolling a second month/leap-year table that would have to agree
// with gregorianDayNumber's by construction instead of by test.
func gregorianDateFromDayNumber(n int) (y, m, d int) {
	t := time.Date(1600, time.January, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
	return t.Year(), int(t.Month()), t.Day()
}
