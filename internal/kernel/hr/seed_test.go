package hr

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

func freshTenantDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	name := fmt.Sprintf("uc_test_hr_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	tenantDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open tenant database %s: %v", name, err)
	}
	t.Cleanup(func() { tenantDB.Close() })
	if err := tenantDB.Ping(); err != nil {
		t.Fatalf("ping tenant database %s: %v", name, err)
	}
	if _, err := tenantDB.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyTenant(context.Background(), tenantDB); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}
	return tenantDB
}

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "hr-test"}
}

func publishedDef(t *testing.T, tenantDB *sql.DB, entityType string) *entity.Definition {
	t.Helper()
	v, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(context.Background(), entityType)
	if err != nil {
		t.Fatalf("GetPublished(%s): %v", entityType, err)
	}
	d, err := entity.Unmarshal(v.Definition)
	if err != nil {
		t.Fatalf("unmarshal %s: %v", entityType, err)
	}
	return d
}

func TestAllDefinitionsValid(t *testing.T) {
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			t.Errorf("%s: %v", def.EntityType, err)
		}
		if def.Module != "hr" {
			t.Errorf("%s declares module %q, want hr", def.EntityType, def.Module)
		}
	}
	for _, f := range AllForms() {
		if err := f.Validate(); err != nil {
			t.Errorf("form %s: %v", f.EntityType, err)
		}
	}

	// ADR-0013's shape, asserted rather than trusted to the comment:
	// Employee links to the person through party_id and carries NO
	// person attributes of its own — the moment a name or contact field
	// appears here, Party has been duplicated and the decision has been
	// silently reversed.
	emp := Employee()
	if f, ok := emp.FieldByName("party_id"); !ok || f.Target != "Party" || !f.Required {
		t.Errorf("Employee.party_id must be a required Party reference, got %+v", f)
	}
	for _, forbidden := range []string{"name", "email", "phone", "preferred_language", "party_type"} {
		if _, ok := emp.FieldByName(forbidden); ok {
			t.Errorf("Employee must not carry the person attribute %q — that lives on Party (ADR-0013)", forbidden)
		}
	}
	// And the HR-domain records point at the EMPLOYMENT, not the person
	// (ADR-0013 rule 4) — attendance is owed under an employment, so a
	// person's two employments have two separate histories.
	for _, def := range []*entity.Definition{LeaveRequest(), AttendanceRecord()} {
		f, ok := def.FieldByName("employee_id")
		if !ok || f.Target != "Employee" {
			t.Errorf("%s.employee_id must target Employee (ADR-0013 rule 4), got %+v", def.EntityType, f)
		}
	}
	// AttendanceRecord has no status graph on purpose: it records what
	// happened, it is not a request anyone decides on.
	if AttendanceRecord().StatusTypeCode != "" {
		t.Error("AttendanceRecord should have no lifecycle — it is a record of fact, not a request")
	}
}

func TestPublish_PublishesEveryDefinition(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := PublishForms(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("PublishForms: %v", err)
	}
	for _, et := range []string{"Employee", "LeaveRequest", "AttendanceRecord"} {
		if got := publishedDef(t, tenantDB, et); got.Module != "hr" {
			t.Errorf("%s published with module %q", et, got.Module)
		}
		if _, err := data.NewFormDefinitionRepo(tenantDB).GetPublished(ctx, et); err != nil {
			t.Errorf("%s form not published: %v", et, err)
		}
	}
	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("re-Publish should be a no-op: %v", err)
	}
}

// The two graphs are different KINDS — a lifecycle and an approval
// chain — and each has one edge that is easy to get wrong.
func TestPublishStatuses_BothGraphs(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	engine := crud.NewEngine(tenantDB)

	flags := map[string]map[string]bool{}
	graph := func(code string) (map[string]string, map[string]bool) {
		t.Helper()
		types, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", code)
		if err != nil || len(types) != 1 {
			t.Fatalf("StatusType %s: err=%v n=%d", code, err, len(types))
		}
		rows, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
		if err != nil {
			t.Fatalf("statuses for %s: %v", code, err)
		}
		byCode := map[string]string{}
		flags[code] = map[string]bool{}
		for _, r := range rows {
			c, _ := r.Data["code"].(string)
			if c == "" {
				continue
			}
			byCode[c] = r.ID
			if v, _ := r.Data["is_initial"].(bool); v {
				flags[code]["initial:"+c] = true
			}
			if v, _ := r.Data["is_terminal"].(bool); v {
				flags[code]["terminal:"+c] = true
			}
		}
		trs, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusTransition"), "status_type_id", types[0].ID)
		if err != nil {
			t.Fatalf("transitions for %s: %v", code, err)
		}
		edges := map[string]bool{}
		for _, tr := range trs {
			from, _ := tr.Data["from_status_id"].(string)
			to, _ := tr.Data["to_status_id"].(string)
			edges[from+"->"+to] = true
		}
		return byCode, edges
	}

	emp, empEdges := graph("employee_status")
	leave, leaveEdges := graph("leave_request_status")

	// Two graphs sharing "cancelled"/"draft"-style codes with other
	// modules must stay scoped to their own status type.
	if emp["active"] == leave["approved"] {
		t.Error("the two graphs must not share Status rows")
	}
	// Extended leave is not a termination: on_leave returns to active.
	if !empEdges[emp["on_leave"]+"->"+emp["active"]] {
		t.Error("an employee on leave must be able to return to active")
	}
	// Leaving can happen from any live state.
	for _, live := range []string{"probation", "active", "on_leave"} {
		if !empEdges[emp[live]+"->"+emp["terminated"]] {
			t.Errorf("%s must be able to reach terminated", live)
		}
	}
	// Nothing returns from terminated — a rehire is a new record
	// (ADR-0013), which is what preserves the first employment's dates.
	if empEdges[emp["terminated"]+"->"+emp["active"]] {
		t.Error("terminated must not transition back — a rehire is a new Employee record")
	}
	// Approved leave can still be withdrawn: plans change, and unlike an
	// approved purchase order no money has moved.
	if !leaveEdges[leave["approved"]+"->"+leave["cancelled"]] {
		t.Error("approved leave must be cancellable")
	}
	if leaveEdges[leave["rejected"]+"->"+leave["submitted"]] {
		t.Error("a rejected request must not walk back to submitted; raise a new one")
	}

	for code, want := range map[string]map[string]bool{
		"employee_status":      {"initial:probation": true, "terminal:terminated": true},
		"leave_request_status": {"initial:draft": true, "terminal:approved": true, "terminal:rejected": true, "terminal:cancelled": true},
	} {
		for flag := range want {
			if !flags[code][flag] {
				t.Errorf("%s is missing %s", code, flag)
			}
		}
		initials := 0
		for f := range flags[code] {
			if strings.HasPrefix(f, "initial:") {
				initials++
			}
		}
		if initials != 1 {
			t.Errorf("%s has %d initial statuses, want exactly 1", code, initials)
		}
	}
}

// TestEmployee_TwoEmploymentsOverTime is ADR-0013's load-bearing
// consequence: one human, two employments, both surviving. If Employee
// were a person master this would be impossible without duplicating the
// person or destroying the first employment's history.
func TestEmployee_TwoEmploymentsOverTime(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"hr", Publish},
		{"hr statuses", PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}
	engine := crud.NewEngine(tenantDB)
	status := func(typeCode, code string) string {
		t.Helper()
		types, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", typeCode)
		if len(types) == 0 {
			t.Fatalf("no StatusType %s", typeCode)
		}
		rows, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
		for _, r := range rows {
			if c, _ := r.Data["code"].(string); c == code {
				return r.ID
			}
		}
		t.Fatalf("no %s/%s", typeCode, code)
		return ""
	}

	person, err := engine.Create(ctx, publishedDef(t, tenantDB, "Party"), map[string]any{
		"party_type": "person", "name": "Demo Employee", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	if _, err := engine.Create(ctx, publishedDef(t, tenantDB, "PartyRole"), map[string]any{
		"party_id": person.ID, "role_type": "employee",
	}, actor); err != nil {
		t.Fatalf("create employee PartyRole: %v", err)
	}

	empDef := publishedDef(t, tenantDB, "Employee")
	// Created in the initial status and then transitioned, rather than
	// straight into terminated: crud.Engine.Create does not run
	// ValidateStatusTransition (internal/api does), so creating directly
	// into a terminal state would exercise a state the product itself
	// cannot produce — the independent review's point.
	first, err := engine.Create(ctx, empDef, map[string]any{
		"employee_number": "EMP-1", "party_id": person.ID,
		"hire_date": "2020-01-06",
		"status_id": status("employee_status", "probation"),
	}, actor)
	if err != nil {
		t.Fatalf("create first employment: %v", err)
	}
	firstVersion := first.Version
	for _, to := range []string{"active", "terminated"} {
		fields := map[string]any{
			"employee_number": "EMP-1", "party_id": person.ID,
			"hire_date": "2020-01-06", "status_id": status("employee_status", to),
		}
		if to == "terminated" {
			fields["end_date"] = "2023-06-30"
		}
		if err := engine.ValidateStatusTransition(ctx, empDef, first.ID, fields, false, &firstVersion); err != nil {
			t.Fatalf("transition to %s should be legal: %v", to, err)
		}
		newVersion, err := engine.Update(ctx, empDef, first.ID, fields, &firstVersion, actor)
		if err != nil {
			t.Fatalf("update to %s: %v", to, err)
		}
		firstVersion = newVersion
	}
	second, err := engine.Create(ctx, empDef, map[string]any{
		"employee_number": "EMP-2", "party_id": person.ID,
		"hire_date": "2026-02-01",
		"status_id": status("employee_status", "probation"),
	}, actor)
	if err != nil {
		t.Fatalf("create rehire employment: %v", err)
	}

	both, err := engine.ListByField(ctx, empDef, "party_id", person.ID)
	if err != nil || len(both) != 2 {
		t.Fatalf("one person should hold two employments: err=%v n=%d", err, len(both))
	}
	// The first employment's dates are intact — the whole point.
	for _, rec := range both {
		if rec.ID == first.ID {
			if rec.Data["end_date"] != "2023-06-30" {
				t.Errorf("the earlier employment lost its end date: %v", rec.Data["end_date"])
			}
		}
	}

	// Leave belongs to an EMPLOYMENT, not the person: a request against
	// the current employment must not appear under the old one.
	leaveDef := publishedDef(t, tenantDB, "LeaveRequest")
	if _, err := engine.Create(ctx, leaveDef, map[string]any{
		"request_number": "LV-1", "employee_id": second.ID, "leave_type": "annual",
		"start_date": "2026-05-01", "end_date": "2026-05-05", "days": 5.0,
		"status_id": status("leave_request_status", "draft"),
	}, actor); err != nil {
		t.Fatalf("create LeaveRequest: %v", err)
	}
	onFirst, err := engine.ListByField(ctx, leaveDef, "employee_id", first.ID)
	if err != nil {
		t.Fatalf("list leave for first employment: %v", err)
	}
	if len(onFirst) != 0 {
		t.Errorf("leave for the current employment leaked onto the previous one: %d rows", len(onFirst))
	}
	onSecond, err := engine.ListByField(ctx, leaveDef, "employee_id", second.ID)
	if err != nil || len(onSecond) != 1 {
		t.Fatalf("current employment's leave: err=%v n=%d", err, len(onSecond))
	}
}

// A leave request whose end date precedes its start must be rejected —
// the NotBefore constraint, which is the only cross-field rule the
// kernel can express today.
func TestLeaveRequest_RejectsReversedDates(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"hr", Publish},
		{"hr statuses", PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}
	engine := crud.NewEngine(tenantDB)
	types, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "leave_request_status")
	rows, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
	var draft string
	for _, r := range rows {
		if c, _ := r.Data["code"].(string); c == "draft" {
			draft = r.ID
		}
	}
	person, _ := engine.Create(ctx, publishedDef(t, tenantDB, "Party"), map[string]any{
		"party_type": "person", "name": "Demo Employee", "status": "active",
	}, actor)
	emp, err := engine.Create(ctx, publishedDef(t, tenantDB, "Employee"), map[string]any{
		"employee_number": "EMP-9", "party_id": person.ID, "hire_date": "2026-01-01",
		"status_id": func() string {
			ts, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "employee_status")
			rs, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", ts[0].ID)
			for _, r := range rs {
				if c, _ := r.Data["code"].(string); c == "active" {
					return r.ID
				}
			}
			return ""
		}(),
	}, actor)
	if err != nil {
		t.Fatalf("create Employee: %v", err)
	}
	if _, err := engine.Create(ctx, publishedDef(t, tenantDB, "LeaveRequest"), map[string]any{
		"request_number": "LV-BAD", "employee_id": emp.ID, "leave_type": "annual",
		"start_date": "2026-05-10", "end_date": "2026-05-01", "days": 5.0,
		"status_id": draft,
	}, actor); err == nil {
		t.Fatal("leave ending before it starts must be rejected")
	}
	// days is bounded to Min:0 (uc-infra#80) — -99 no longer saves.
	// No Max: how many days a request can span is tenant policy
	// (ADR-0018 §Consequences), not a kernel-invariant ceiling.
	if _, err := engine.Create(ctx, publishedDef(t, tenantDB, "LeaveRequest"), map[string]any{
		"request_number": "LV-NEG", "employee_id": emp.ID, "leave_type": "annual",
		"start_date": "2026-05-01", "end_date": "2026-05-05", "days": -99.0,
		"status_id": draft,
	}, actor); err == nil {
		t.Fatal("a negative days must be rejected")
	}
}

// Publishing statuses before foundation is a real operator ordering
// mistake and must fail cleanly.
func TestPublishStatuses_RequiresFoundation(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	err := PublishStatuses(ctx, tenantDB, humanActor())
	if err == nil {
		t.Fatal("PublishStatuses without foundation should fail")
	}
	if !strings.Contains(err.Error(), "StatusType") {
		t.Errorf("error should name the missing definition, got %v", err)
	}
}

// The Employee date chain, and the two value constraints that ARE
// enforced — the package comment enumerates what is not, so what is
// should be pinned.
func TestEmployee_RejectsReversedEmploymentDates(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"hr", Publish},
		{"hr statuses", PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}
	engine := crud.NewEngine(tenantDB)
	types, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "employee_status")
	rows, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
	var probation string
	for _, r := range rows {
		if c, _ := r.Data["code"].(string); c == "probation" {
			probation = r.ID
		}
	}
	person, _ := engine.Create(ctx, publishedDef(t, tenantDB, "Party"), map[string]any{
		"party_type": "person", "name": "Demo Employee", "status": "active",
	}, actor)

	if _, err := engine.Create(ctx, publishedDef(t, tenantDB, "Employee"), map[string]any{
		"employee_number": "EMP-BAD", "party_id": person.ID,
		"hire_date": "2026-06-01", "end_date": "2026-01-01",
		"status_id": probation,
	}, actor); err == nil {
		t.Fatal("an employment ending before it started must be rejected")
	}

	// An unknown leave_type must be rejected by the enum — the other
	// constraint this module actually enforces.
	emp, err := engine.Create(ctx, publishedDef(t, tenantDB, "Employee"), map[string]any{
		"employee_number": "EMP-OK", "party_id": person.ID,
		"hire_date": "2026-01-01", "status_id": probation,
	}, actor)
	if err != nil {
		t.Fatalf("create Employee: %v", err)
	}
	leaveTypes, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "leave_request_status")
	leaveRows, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", leaveTypes[0].ID)
	var draft string
	for _, r := range leaveRows {
		if c, _ := r.Data["code"].(string); c == "draft" {
			draft = r.ID
		}
	}
	if _, err := engine.Create(ctx, publishedDef(t, tenantDB, "LeaveRequest"), map[string]any{
		"request_number": "LV-X", "employee_id": emp.ID, "leave_type": "sabbatical",
		"start_date": "2026-05-01", "end_date": "2026-05-02", "days": 2.0,
		"status_id": draft,
	}, actor); err == nil {
		t.Fatal("an undeclared leave_type must be rejected by the enum")
	}
}

// Re-running PublishStatuses must not duplicate rows — only re-Publish
// was covered before.
func TestPublishStatuses_Idempotent(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"hr", Publish},
		{"hr statuses", PublishStatuses},
		{"hr statuses again", PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
	}
	engine := crud.NewEngine(tenantDB)
	for code, want := range map[string]int{"employee_status": 4, "leave_request_status": 5} {
		types, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", code)
		if len(types) != 1 {
			t.Fatalf("%s: expected one StatusType, got %d", code, len(types))
		}
		rows, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
		if len(rows) != want {
			t.Errorf("%s: re-seed duplicated statuses: %d rows, want %d", code, len(rows), want)
		}
	}
}

// TestAttendanceRecord_BelongsToTheEmployment is ADR-0013 rule 4 for
// attendance: two employments of the same person keep separate
// histories, which is the whole reason attendance references Employee
// rather than Party.
func TestAttendanceRecord_BelongsToTheEmployment(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"hr", Publish},
		{"hr statuses", PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}
	engine := crud.NewEngine(tenantDB)
	types, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "employee_status")
	rows, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
	var probation string
	for _, r := range rows {
		if c, _ := r.Data["code"].(string); c == "probation" {
			probation = r.ID
		}
	}
	person, _ := engine.Create(ctx, publishedDef(t, tenantDB, "Party"), map[string]any{
		"party_type": "person", "name": "Demo Employee", "status": "active",
	}, actor)

	empDef := publishedDef(t, tenantDB, "Employee")
	older, err := engine.Create(ctx, empDef, map[string]any{
		"employee_number": "EMP-OLD", "party_id": person.ID,
		"hire_date": "2019-01-07", "status_id": probation,
	}, actor)
	if err != nil {
		t.Fatalf("create first employment: %v", err)
	}
	current, err := engine.Create(ctx, empDef, map[string]any{
		"employee_number": "EMP-NEW", "party_id": person.ID,
		"hire_date": "2026-02-02", "status_id": probation,
	}, actor)
	if err != nil {
		t.Fatalf("create second employment: %v", err)
	}

	attDef := publishedDef(t, tenantDB, "AttendanceRecord")
	if _, err := engine.Create(ctx, attDef, map[string]any{
		"employee_id": current.ID, "entry_date": "2026-07-28",
		"hours_worked": 8.0, "source": "clock",
	}, actor); err != nil {
		t.Fatalf("create AttendanceRecord: %v", err)
	}

	onOlder, err := engine.ListByField(ctx, attDef, "employee_id", older.ID)
	if err != nil {
		t.Fatalf("list attendance for the older employment: %v", err)
	}
	if len(onOlder) != 0 {
		t.Errorf("attendance for the current employment leaked onto the previous one: %d rows", len(onOlder))
	}
	onCurrent, err := engine.ListByField(ctx, attDef, "employee_id", current.ID)
	if err != nil || len(onCurrent) != 1 {
		t.Fatalf("current employment's attendance: err=%v n=%d", err, len(onCurrent))
	}

	// The source enum is enforced.
	if _, err := engine.Create(ctx, attDef, map[string]any{
		"employee_id": current.ID, "entry_date": "2026-07-29",
		"hours_worked": 8.0, "source": "telepathy",
	}, actor); err == nil {
		t.Error("an undeclared attendance source must be rejected by the enum")
	}
}

// hours_worked is bounded to [0, 24] (uc-infra#80) — a negative or
// 30-hour day no longer saves cleanly. Reversion check performed by hand
// against the pre-fix code (git stash of validate.go's Min/Max branch):
// both cases below passed with no error before the fix.
func TestAttendanceRecord_HoursWorkedBounded(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"hr", Publish},
		{"hr statuses", PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}
	engine := crud.NewEngine(tenantDB)
	types, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "employee_status")
	rows, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
	var active string
	for _, r := range rows {
		if c, _ := r.Data["code"].(string); c == "active" {
			active = r.ID
		}
	}
	person, _ := engine.Create(ctx, publishedDef(t, tenantDB, "Party"), map[string]any{
		"party_type": "person", "name": "Demo Employee", "status": "active",
	}, actor)
	emp, err := engine.Create(ctx, publishedDef(t, tenantDB, "Employee"), map[string]any{
		"employee_number": "EMP-HRS", "party_id": person.ID, "hire_date": "2026-01-01",
		"status_id": active,
	}, actor)
	if err != nil {
		t.Fatalf("create Employee: %v", err)
	}
	attDef := publishedDef(t, tenantDB, "AttendanceRecord")

	if _, err := engine.Create(ctx, attDef, map[string]any{
		"employee_id": emp.ID, "entry_date": "2026-07-28",
		"hours_worked": -1.0, "source": "manual",
	}, actor); err == nil {
		t.Error("a negative hours_worked must be rejected")
	}
	if _, err := engine.Create(ctx, attDef, map[string]any{
		"employee_id": emp.ID, "entry_date": "2026-07-28",
		"hours_worked": 30.0, "source": "manual",
	}, actor); err == nil {
		t.Error("a 30-hour day must be rejected")
	}
	// The bound is inclusive: exactly 0 and exactly 24 both save.
	if _, err := engine.Create(ctx, attDef, map[string]any{
		"employee_id": emp.ID, "entry_date": "2026-07-28",
		"hours_worked": 0.0, "source": "manual",
	}, actor); err != nil {
		t.Errorf("hours_worked of exactly 0 (the inclusive minimum) should save: %v", err)
	}
	if _, err := engine.Create(ctx, attDef, map[string]any{
		"employee_id": emp.ID, "entry_date": "2026-07-29",
		"hours_worked": 24.0, "source": "manual",
	}, actor); err != nil {
		t.Errorf("hours_worked of exactly 24 (the inclusive maximum) should save: %v", err)
	}
}
