package workflow

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron expression support for scheduled workflows (R18) — the schedule
// half of "a schedule definition feeding the EXISTING workflow queue, not
// a second scheduling system".
//
// Hand-written rather than pulling in a cron library, for one specific
// reason: this decides WHEN business processes fire, which makes it the
// same category of code as internal/kernel/ledger — a small, deterministic
// core whose behaviour has to be exactly predictable and exhaustively
// testable, with no surprises from a dependency's own interpretation of
// ambiguous cases. It is deliberately a STRICT SUBSET of cron rather than
// a permissive one: only the five standard fields, and only `*`, a number,
// a `a-b` range, an `a,b,c` list, and a `*/n` or `a-b/n` step. No @yearly
// aliases, no `L`/`W`/`#`, no seconds field, no named months or weekdays.
//
// Rejecting what it does not implement matters more than accepting it.
// An unsupported token silently treated as "*" would turn a
// once-a-month job into an every-minute one, and the failure would be a
// runaway rather than a stoppage — so anything unrecognised is an error at
// publish time (Definition.Validate), not a surprise at 3am.
//
// All evaluation is in UTC. A tenant-local schedule is a real future
// requirement (a "nightly" run means the tenant's night), but doing it
// properly needs a per-tenant timezone that does not exist anywhere in
// this kernel yet, and guessing one is worse than a documented limitation:
// DST alone turns a naive local implementation into a job that silently
// runs twice or not at all, once a year.

// maxScheduleGapYears bounds Next's search — see its doc comment for why
// ten rather than five.
const maxScheduleGapYears = 10

// validationEpoch is the FIXED reference Definition.Validate searches
// from when proving a schedule can ever occur. Deliberately not
// time.Now(): validation that reads the clock is not reproducible, and an
// unchanged Definition could pass today and fail decades later purely
// from the passage of time (independent review proved exactly that for a
// leap-day schedule). 2000 is a leap year — divisible by 400 — so a
// ten-year window from here contains 2000, 2004 and 2008, and any
// expression that can occur at all occurs within it.
var validationEpoch = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// cronField is one parsed field: the set of values it matches.
type cronField struct {
	// bits[i] is true when value min+i matches. A set rather than a
	// re-parsed expression so Next is a lookup, not a re-interpretation.
	bits []bool
	min  int
}

func (f cronField) matches(v int) bool {
	i := v - f.min
	if i < 0 || i >= len(f.bits) {
		return false
	}
	return f.bits[i]
}

// Schedule is a parsed cron expression, ready to answer "when next?".
type Schedule struct {
	minute, hour, dom, month, dow cronField
	// domRestricted/dowRestricted record whether the day-of-month and
	// day-of-week fields were literally "*". Cron's day handling is the
	// one genuinely surprising rule in the format and it is deliberate:
	// when BOTH are restricted the day matches if EITHER does (an OR, not
	// an AND), which is why "0 0 13 * 5" means the 13th *and* every
	// Friday, not Friday the 13th. Implementing that as an AND is the
	// classic way to write a schedule that almost never fires.
	domRestricted, dowRestricted bool
	expr                         string
}

func (s Schedule) String() string { return s.expr }

// ParseSchedule parses a 5-field cron expression
// (minute hour day-of-month month day-of-week) in UTC.
func ParseSchedule(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("cron expression %q must have exactly 5 fields (minute hour day-of-month month day-of-week), got %d", expr, len(fields))
	}
	specs := []struct {
		name     string
		min, max int
	}{
		{"minute", 0, 59},
		{"hour", 0, 23},
		{"day-of-month", 1, 31},
		{"month", 1, 12},
		{"day-of-week", 0, 6},
	}
	var parsed [5]cronField
	for i, spec := range specs {
		f, err := parseCronField(fields[i], spec.min, spec.max)
		if err != nil {
			return Schedule{}, fmt.Errorf("cron expression %q: %s field: %w", expr, spec.name, err)
		}
		parsed[i] = f
	}
	return Schedule{
		minute: parsed[0], hour: parsed[1], dom: parsed[2], month: parsed[3], dow: parsed[4],
		domRestricted: fields[2] != "*",
		dowRestricted: fields[4] != "*",
		expr:          strings.Join(fields, " "),
	}, nil
}

// atoiStrict is strconv.Atoi minus the leading-sign tolerance. Go's Atoi
// accepts "+5" and "-5"; neither appears in this format's documented
// subset, and quietly accepting "+5" while claiming to reject everything
// unimplemented is the kind of small dishonesty that makes the rest of
// the strictness claim untrustworthy. (Independent review found it.)
func atoiStrict(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q is not a plain non-negative number", s)
		}
	}
	return strconv.Atoi(s)
}

func parseCronField(s string, min, max int) (cronField, error) {
	f := cronField{bits: make([]bool, max-min+1), min: min}
	if s == "" {
		return cronField{}, fmt.Errorf("empty")
	}
	for _, part := range strings.Split(s, ",") {
		lo, hi, step := min, max, 1

		if base, stepStr, found := strings.Cut(part, "/"); found {
			n, err := atoiStrict(stepStr)
			if err != nil || n < 1 {
				return cronField{}, fmt.Errorf("step in %q must be a positive integer", part)
			}
			step = n
			part = base
			// "*/5" and "0-30/5" both legal; a bare "5/2" is not — a step
			// needs a range to step through, and reading it as "from 5
			// onwards" is a guess other cron implementations disagree on.
			if part != "*" && !strings.Contains(part, "-") {
				return cronField{}, fmt.Errorf("step %q needs a range or * to step over", s)
			}
		}

		switch {
		case part == "*":
			// lo/hi already the full range
		case strings.Contains(part, "-"):
			a, b, _ := strings.Cut(part, "-")
			var err error
			if lo, err = atoiStrict(a); err != nil {
				return cronField{}, fmt.Errorf("range start %q is not a number", a)
			}
			if hi, err = atoiStrict(b); err != nil {
				return cronField{}, fmt.Errorf("range end %q is not a number", b)
			}
			if lo > hi {
				return cronField{}, fmt.Errorf("range %q starts after it ends", part)
			}
		default:
			n, err := atoiStrict(part)
			if err != nil {
				return cronField{}, fmt.Errorf("%q is not a number, a range, or *", part)
			}
			lo, hi = n, n
		}

		if lo < min || hi > max {
			return cronField{}, fmt.Errorf("%q is outside the valid range %d-%d", part, min, max)
		}
		for v := lo; v <= hi; v += step {
			f.bits[v-min] = true
		}
	}
	return f, nil
}

// matchesDay applies cron's OR rule for the two day fields — see the
// Schedule struct's own comment on why this is not an AND.
func (s Schedule) matchesDay(t time.Time) bool {
	dom := s.dom.matches(t.Day())
	dow := s.dow.matches(int(t.Weekday()))
	switch {
	case s.domRestricted && s.dowRestricted:
		return dom || dow
	case s.domRestricted:
		return dom
	case s.dowRestricted:
		return dow
	default:
		return true
	}
}

// Next returns the first time strictly after `after` that this schedule
// matches, in UTC.
//
// Strictly after, never equal: the caller records the time it last fired
// and asks for the next one, so returning `after` itself would make a
// schedule fire forever on the same minute.
//
// The bound exists because an expression like "0 0 30 2 *" — the 30th of
// February — parses as perfectly valid cron and simply never occurs;
// without one this would spin forever. Returning an error makes an
// impossible schedule visible instead of hanging.
//
// Ten years, not five. The longest gap a LEGITIMATE expression can have
// is eight years: "0 0 29 2 *" fires every four years normally, but a
// century year not divisible by 400 is not a leap year, so 2096 jumps
// straight to 2104. A five-year bound rejected that as impossible —
// found by independent review, which proved it by asking for the next
// occurrence after 2096. Ten leaves margin over the real eight-year
// worst case without making a genuinely impossible expression take
// meaningfully longer to detect.
func (s Schedule) Next(after time.Time) (time.Time, error) {
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(maxScheduleGapYears, 0, 0)
	for t.Before(limit) {
		if !s.month.matches(int(t.Month())) {
			// Jump to the 1st of next month rather than stepping a minute
			// at a time — the difference between a bounded search and
			// scanning half a million minutes to answer "next February".
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
			continue
		}
		if !s.matchesDay(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
			continue
		}
		if !s.hour.matches(t.Hour()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC).Add(time.Hour)
			continue
		}
		if !s.minute.matches(t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cron expression %q has no occurrence within %d years of %s — is it a date that never happens, such as 30 February?", s.expr, maxScheduleGapYears, after.UTC().Format(time.RFC3339))
}
