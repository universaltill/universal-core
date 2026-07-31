package workflow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) Schedule {
	t.Helper()
	s, err := ParseSchedule(expr)
	if err != nil {
		t.Fatalf("ParseSchedule(%q): %v", expr, err)
	}
	return s
}

func at(s string) time.Time {
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tm.UTC()
}

func TestSchedule_Next(t *testing.T) {
	cases := []struct {
		name, expr, from, want string
	}{
		{"every minute", "* * * * *", "2026-03-01T10:00:00Z", "2026-03-01T10:01:00Z"},
		{"nightly 3am, later today", "0 3 * * *", "2026-03-01T00:10:00Z", "2026-03-01T03:00:00Z"},
		{"nightly 3am, rolls to tomorrow", "0 3 * * *", "2026-03-01T03:00:00Z", "2026-03-02T03:00:00Z"},
		{"top of every hour", "0 * * * *", "2026-03-01T10:30:00Z", "2026-03-01T11:00:00Z"},
		{"every 15 minutes", "*/15 * * * *", "2026-03-01T10:07:00Z", "2026-03-01T10:15:00Z"},
		{"step wraps the hour", "*/15 * * * *", "2026-03-01T10:47:00Z", "2026-03-01T11:00:00Z"},
		{"list of minutes", "0,30 * * * *", "2026-03-01T10:05:00Z", "2026-03-01T10:30:00Z"},
		{"range of hours", "0 9-17 * * *", "2026-03-01T08:00:00Z", "2026-03-01T09:00:00Z"},
		{"range of hours, past the end", "0 9-17 * * *", "2026-03-01T18:00:00Z", "2026-03-02T09:00:00Z"},
		{"first of the month", "0 0 1 * *", "2026-03-15T00:00:00Z", "2026-04-01T00:00:00Z"},
		{"specific month only", "0 0 1 2 *", "2026-03-01T00:00:00Z", "2027-02-01T00:00:00Z"},
		{"weekday only (Monday)", "0 0 * * 1", "2026-03-01T00:00:00Z", "2026-03-02T00:00:00Z"},
		{"sunday is 0", "0 0 * * 0", "2026-03-02T00:00:00Z", "2026-03-08T00:00:00Z"},
		{"leap day exists in 2028", "0 0 29 2 *", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		{"stepped range", "0-30/10 * * * *", "2026-03-01T10:01:00Z", "2026-03-01T10:10:00Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := mustParse(t, c.expr).Next(at(c.from))
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if got.Format(time.RFC3339) != c.want {
				t.Fatalf("%q from %s: got %s, want %s", c.expr, c.from, got.Format(time.RFC3339), c.want)
			}
		})
	}
}

// Cron's day rule is its one genuinely surprising behaviour, and getting
// it backwards is the classic way to write a schedule that almost never
// fires. When BOTH day-of-month and day-of-week are restricted the day
// matches if EITHER does — so "the 13th, and every Friday", NOT "Friday
// the 13th".
func TestSchedule_DayOfMonthAndDayOfWeekAreOred(t *testing.T) {
	s := mustParse(t, "0 0 13 * 5") // 13th OR Friday
	// 2026-03-01 is a Sunday; the first Friday is the 6th, well before
	// the 13th. An AND implementation would skip it and wait for a
	// Friday that also falls on the 13th.
	got, err := s.Next(at("2026-03-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got.Format(time.RFC3339) != "2026-03-06T00:00:00Z" {
		t.Fatalf("expected the first Friday (6 Mar), got %s — day fields are being ANDed, not ORed", got.Format(time.RFC3339))
	}
	// And the 13th itself matches even though 2026-03-13 is a Friday
	// anyway; use a month where the 13th is NOT a Friday to prove the
	// day-of-month arm independently.
	got, err = mustParse(t, "0 0 13 * 5").Next(at("2026-04-04T00:00:00Z"))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	// April 2026: Fridays are 3,10,17,24 — the 10th comes before the 13th.
	if got.Format(time.RFC3339) != "2026-04-10T00:00:00Z" {
		t.Fatalf("got %s", got.Format(time.RFC3339))
	}
}

// Strictly after, never equal — the caller stores "last fired" and asks
// for the next one, so returning the same instant would make a schedule
// fire forever on one minute.
func TestSchedule_NextIsStrictlyAfter(t *testing.T) {
	s := mustParse(t, "0 3 * * *")
	from := at("2026-03-01T03:00:00Z")
	got, err := s.Next(from)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !got.After(from) {
		t.Fatalf("Next(%s) = %s — must be strictly after", from, got)
	}
}

// Sub-minute precision must not cause a double fire within the same
// minute: 03:00:30 should advance to tomorrow, not back to 03:00:00.
func TestSchedule_IgnoresSubMinutePrecision(t *testing.T) {
	got, err := mustParse(t, "0 3 * * *").Next(at("2026-03-01T03:00:30Z"))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got.Format(time.RFC3339) != "2026-03-02T03:00:00Z" {
		t.Fatalf("got %s, want the next day", got.Format(time.RFC3339))
	}
}

// A date that parses but never occurs must error rather than spin. This
// is the guard that stops an impossible schedule hanging a worker.
func TestSchedule_ImpossibleDateErrorsRatherThanHanging(t *testing.T) {
	_, err := mustParse(t, "0 0 30 2 *").Next(at("2026-03-01T00:00:00Z")) // 30 February
	if err == nil {
		t.Fatal("30 February should have no occurrence and must report that, not hang or return a wrong date")
	}
	if !strings.Contains(err.Error(), "30 February") {
		t.Fatalf("the error should name the likely cause, got: %v", err)
	}
}

// Rejecting what it does not implement is the point: anything silently
// treated as "*" would turn a rare job into an every-minute one, and the
// failure mode would be a runaway rather than a stoppage.
func TestParseSchedule_RejectsBadExpressions(t *testing.T) {
	for _, expr := range []string{
		"",
		"* * * *",      // 4 fields
		"* * * * * *",  // 6 fields (seconds not supported)
		"60 * * * *",   // minute out of range
		"* 24 * * *",   // hour out of range
		"* * 0 * *",    // day-of-month is 1-based
		"* * 32 * *",   // day-of-month out of range
		"* * * 13 *",   // month out of range
		"* * * * 7",    // day-of-week is 0-6
		"@daily",       // aliases not supported
		"0 0 L * *",    // last-day not supported
		"0 0 * * MON",  // named weekdays not supported
		"*/0 * * * *",  // zero step
		"*/-1 * * * *", // negative step
		"5/2 * * * *",  // step with no range to step over
		"10-5 * * * *", // inverted range
		"a * * * *",    // not a number
	} {
		if _, err := ParseSchedule(expr); err == nil {
			t.Errorf("ParseSchedule(%q) should have been rejected but was accepted", expr)
		}
	}
}

func TestParseSchedule_NormalisesWhitespace(t *testing.T) {
	s, err := ParseSchedule("  0   3 * * *  ")
	if err != nil {
		t.Fatalf("extra whitespace should be tolerated: %v", err)
	}
	if s.String() != "0 3 * * *" {
		t.Fatalf("String() = %q, want the normalised form", s.String())
	}
}

// A scheduled workflow must be rejected at PUBLISH time for a bad
// expression — not discovered at fire time, when the symptom is a job
// that silently never runs.
func TestDefinitionValidate_ScheduledTrigger(t *testing.T) {
	def := func(cron string) *Definition {
		return &Definition{
			Name: "nightly", Version: 1,
			Trigger: Trigger{Type: TriggerScheduled, Cron: cron},
			Steps:   []Step{{Kind: StepNotify}},
		}
	}
	if err := def("0 3 * * *").Validate(); err != nil {
		t.Fatalf("a valid schedule should publish: %v", err)
	}
	if err := def("").Validate(); err == nil {
		t.Fatal("a scheduled trigger with no cron expression must be rejected")
	}
	if err := def("not a cron").Validate(); err == nil {
		t.Fatal("a malformed cron expression must be rejected at publish time")
	}
	// Parses fine, never occurs — refused rather than published as a job
	// that would silently never fire.
	if err := def("0 0 30 2 *").Validate(); err == nil {
		t.Fatal("a schedule that can never occur must be rejected at publish time")
	}
}

// Round-trips through JSON, since Definitions are stored and reloaded as
// JSON in the registry — a trigger field that does not survive that is
// useless however well it parses.
func TestScheduledTrigger_SurvivesJSONRoundTrip(t *testing.T) {
	original := &Definition{
		Name: "nightly_reconciliation", Version: 1,
		Trigger: Trigger{Type: TriggerScheduled, Cron: "0 3 * * *"},
		Steps:   []Step{{Kind: StepNotify}},
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := Unmarshal(raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Trigger.Type != TriggerScheduled || back.Trigger.Cron != "0 3 * * *" {
		t.Fatalf("trigger did not survive the round trip: %+v", back.Trigger)
	}
}

// Regression for a bound that was provably too small. A leap-day
// schedule normally repeats every four years, but a century year not
// divisible by 400 is not a leap year — so 2096 jumps straight to 2104,
// an eight-year gap. The original five-year bound rejected that
// legitimate schedule as impossible, and said so with a message blaming
// "a date that never happens". Found by independent review.
func TestSchedule_LeapDayAcrossANonLeapCentury(t *testing.T) {
	got, err := mustParse(t, "0 0 29 2 *").Next(at("2096-03-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("2104-02-29 is a real occurrence and must be found: %v", err)
	}
	if got.Format(time.RFC3339) != "2104-02-29T00:00:00Z" {
		t.Fatalf("got %s, want 2104-02-29 (2100 is not a leap year)", got.Format(time.RFC3339))
	}
}

// Validation must not depend on the wall clock. An unchanged Definition
// that publishes today has to publish in eighty years too — independent
// review proved the time.Now()-based version would reject a leap-day
// schedule if merely re-run in 2097.
func TestDefinitionValidate_IsNotTimeDependent(t *testing.T) {
	def := &Definition{
		Name: "leap_day_only", Version: 1,
		Trigger: Trigger{Type: TriggerScheduled, Cron: "0 0 29 2 *"},
		Steps:   []Step{{Kind: StepNotify}},
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("a leap-day schedule is legitimate and must validate: %v", err)
	}
	// The epoch is fixed, so this is the same answer forever — asserted
	// by checking the search starts from the constant, not from now.
	if validationEpoch.Year() != 2000 {
		t.Fatalf("validation epoch moved to %d — it must stay fixed", validationEpoch.Year())
	}
}

// A trigger carrying a field belonging to a different trigger type is
// almost always a UI that switched type without clearing the old value.
// Silently ignoring it makes the Definition look validated while
// carrying stale, meaningless data.
func TestDefinitionValidate_RejectsCrossedTriggerFields(t *testing.T) {
	cases := []struct {
		name    string
		trigger Trigger
	}{
		{"scheduled with a stray entity_type", Trigger{Type: TriggerScheduled, Cron: "0 3 * * *", EntityType: "PurchaseOrder"}},
		{"on_create with a stray cron", Trigger{Type: TriggerOnCreate, EntityType: "PurchaseOrder", Cron: "0 3 * * *"}},
		{"manual with a stray cron", Trigger{Type: TriggerManual, Cron: "0 3 * * *"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Definition{Name: "x", Version: 1, Trigger: c.trigger, Steps: []Step{{Kind: StepNotify}}}
			if err := d.Validate(); err == nil {
				t.Fatal("should have been rejected")
			}
		})
	}
}

// Go's strconv.Atoi tolerates a leading sign; the documented subset does
// not include one. Accepting "+5" while claiming to reject everything
// unimplemented undermines the strictness the rest of this file relies on.
func TestParseSchedule_RejectsSignedNumbers(t *testing.T) {
	for _, expr := range []string{"+5 * * * *", "*/+5 * * * *", "0-+5 * * * *", "-5 * * * *"} {
		if _, err := ParseSchedule(expr); err == nil {
			t.Errorf("ParseSchedule(%q) should be rejected — signs are not in the supported subset", expr)
		}
	}
}

// The department param on require_approval is validated at publish time
// (R17 routing): a non-string or empty value, or one with no role to
// scope, must be rejected rather than silently ignored at approval time.
func TestDefinitionValidate_DepartmentParam(t *testing.T) {
	step := func(params map[string]any) *Definition {
		return &Definition{Name: "w", Version: 1,
			Trigger: Trigger{Type: TriggerManual},
			Steps:   []Step{{Kind: StepRequireApproval, Params: params}}}
	}
	if err := step(map[string]any{"role": "fm", "department": "department_id"}).Validate(); err != nil {
		t.Fatalf("a valid role+department should publish: %v", err)
	}
	if err := step(map[string]any{"role": "fm", "department": ""}).Validate(); err == nil {
		t.Fatal("empty department field must be rejected")
	}
	if err := step(map[string]any{"role": "fm", "department": 5}).Validate(); err == nil {
		t.Fatal("non-string department must be rejected")
	}
	if err := step(map[string]any{"department": "department_id"}).Validate(); err == nil {
		t.Fatal("department with no role to scope must be rejected")
	}
}
