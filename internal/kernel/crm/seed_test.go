package crm

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

	name := fmt.Sprintf("uc_test_crm_%d", time.Now().UnixNano())
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
	return audit.Actor{Type: audit.ActorHuman, ID: "crm-test"}
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
		if def.Module != "crm" {
			t.Errorf("%s declares module %q, want crm", def.EntityType, def.Module)
		}
	}
	for _, f := range AllForms() {
		if err := f.Validate(); err != nil {
			t.Errorf("form %s: %v", f.EntityType, err)
		}
	}

	c := Case()
	// Case has neither "name" nor "title", so without an explicit label
	// it would render as a raw UUID wherever it is referenced and its
	// picker would be unsearchable — #16's blocker, and ADR-0013's
	// closing rule.
	if c.LabelField == "" {
		t.Error("Case must declare a LabelField: it has no name/title for the convention to find")
	}
	if _, ok := c.FieldByName(c.LabelField); !ok {
		t.Errorf("LabelField %q is not a field of Case", c.LabelField)
	}
	// The agent is a Party, not an hr.Employee — support is routinely
	// handled by contractors (ADR-0013 rule 4).
	if f, ok := c.FieldByName("assignee_id"); !ok || f.Target != "Party" {
		t.Errorf("Case.assignee_id must target Party (ADR-0013 rule 4), got %+v", f)
	}
	// The cross-module context fields must be optional: a tenant can
	// license CRM without sales, and plenty of cases concern no
	// specific product or order.
	for _, name := range []string{"item_id", "sales_order_id"} {
		f, ok := c.FieldByName(name)
		if !ok {
			t.Fatalf("Case has no %s field", name)
		}
		if f.Required {
			t.Errorf("Case.%s must be optional — CRM can be licensed without sales, and not every case has one", name)
		}
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
	if got := publishedDef(t, tenantDB, "Case"); got.Module != "crm" {
		t.Errorf("Case published with module %q", got.Module)
	}
	if _, err := data.NewFormDefinitionRepo(tenantDB).GetPublished(ctx, "Case"); err != nil {
		t.Errorf("Case form not published: %v", err)
	}
	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("re-Publish should be a no-op: %v", err)
	}
}

// TestPublish_WorksWithoutSalesLicensed is the cross-module claim the
// package comment makes: Case references Item and SalesOrder, and a
// tenant may have neither module. Publishing must still work, because
// Validate checks a reference's shape rather than whether its target
// exists.
func TestPublish_WorksWithoutSalesLicensed(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	// Deliberately NO purchasing/sales publish.
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("crm must publish without sales licensed: %v", err)
	}
	if err := PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}

	// And a case can be raised with no product or order context.
	engine := crud.NewEngine(tenantDB)
	types, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "case_status")
	rows, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
	var newStatus string
	for _, r := range rows {
		if c, _ := r.Data["code"].(string); c == "new" {
			newStatus = r.ID
		}
	}
	customer, err := engine.Create(ctx, publishedDef(t, tenantDB, "Party"), map[string]any{
		"party_type": "organization", "name": "Demo Customer", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	if _, err := engine.Create(ctx, publishedDef(t, tenantDB, "Case"), map[string]any{
		"case_number": "CASE-1", "subject": "Account access problem",
		"customer_id": customer.ID, "priority": "normal",
		"opened_date": "2026-08-01", "status_id": newStatus,
	}, actor); err != nil {
		t.Fatalf("create Case without product/order context: %v", err)
	}
}

// TestReferenceIDsMatching_UnpublishedTargetDegrades: the module's
// central claim is that CRM can be licensed without sales, and the
// publish half was the only half tested. Filtering a Case list by
// item_id in such a tenant used to 500, because the target-definition
// lookup propagated ErrNotFound into the page error instead of
// degrading the way every neighbouring resolution path does
// (independent review). Covered here at the kernel layer; the HTTP
// layer's own behaviour is covered by internal/api's list tests.
func TestReferenceIDsMatching_UnpublishedTargetDegrades(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Item lives in purchasing, which this tenant does not have.
	if _, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(ctx, "Item"); err == nil {
		t.Fatal("this test needs a tenant WITHOUT purchasing published")
	}
	// The Case Definition still references it, and that must not be an
	// error anywhere — publishing already proved that above; this pins
	// that the reference is genuinely to an absent target.
	c := publishedDef(t, tenantDB, "Case")
	f, ok := c.FieldByName("item_id")
	if !ok || f.Target != "Item" {
		t.Fatalf("Case.item_id should reference Item, got %+v", f)
	}
}

// The status graph's two load-bearing edges: a case can wait on the
// customer and come back, and a resolved case can reopen — the
// customer decides whether it is fixed, and forcing a new case would
// scatter one problem's history and flatter every resolution-time
// measure.
func TestPublishStatuses_SupportWorkflow(t *testing.T) {
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
	types, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "case_status")
	if err != nil || len(types) != 1 {
		t.Fatalf("case_status StatusType: err=%v n=%d", err, len(types))
	}
	rows, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
	if err != nil || len(rows) != 6 {
		t.Fatalf("statuses: err=%v n=%d, want 6", err, len(rows))
	}
	byCode := map[string]string{}
	initials, terminals := 0, map[string]bool{}
	for _, r := range rows {
		c, _ := r.Data["code"].(string)
		byCode[c] = r.ID
		if v, _ := r.Data["is_initial"].(bool); v {
			initials++
		}
		if v, _ := r.Data["is_terminal"].(bool); v {
			terminals[c] = true
		}
	}
	if initials != 1 {
		t.Errorf("case_status has %d initial statuses, want exactly 1", initials)
	}
	if !terminals["closed"] || !terminals["cancelled"] {
		t.Errorf("closed and cancelled must both be terminal, got %v", terminals)
	}

	trs, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusTransition"), "status_type_id", types[0].ID)
	edges := map[string]bool{}
	for _, tr := range trs {
		from, _ := tr.Data["from_status_id"].(string)
		to, _ := tr.Data["to_status_id"].(string)
		edges[from+"->"+to] = true
	}
	has := func(a, b string) bool { return edges[byCode[a]+"->"+byCode[b]] }
	if !has("in_progress", "waiting_customer") || !has("waiting_customer", "in_progress") {
		t.Error("a case must be able to wait on the customer and come back")
	}
	if !has("resolved", "in_progress") {
		t.Error("a resolved case must be able to reopen — the customer decides whether it is fixed")
	}
	if has("closed", "in_progress") {
		t.Error("closed is terminal; reopening a closed case must be a new case")
	}

	// Idempotent re-seed.
	if err := PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("re-PublishStatuses: %v", err)
	}
	again, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
	if len(again) != 6 {
		t.Errorf("re-seed duplicated statuses: %d rows", len(again))
	}
}

// sla_due_at must not precede the day the case was opened — the one
// cross-field rule the kernel can express, and the priority enum.
func TestCase_RejectsBadInput(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"crm", Publish},
		{"crm statuses", PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}
	engine := crud.NewEngine(tenantDB)
	types, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "case_status")
	rows, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
	var newStatus string
	for _, r := range rows {
		if c, _ := r.Data["code"].(string); c == "new" {
			newStatus = r.ID
		}
	}
	customer, _ := engine.Create(ctx, publishedDef(t, tenantDB, "Party"), map[string]any{
		"party_type": "organization", "name": "Demo Customer", "status": "active",
	}, actor)
	caseDef := publishedDef(t, tenantDB, "Case")

	if _, err := engine.Create(ctx, caseDef, map[string]any{
		"case_number": "CASE-BAD", "subject": "Backdated SLA", "customer_id": customer.ID,
		"priority": "normal", "opened_date": "2026-08-01", "sla_due_at": "2026-07-01",
		"status_id": newStatus,
	}, actor); err == nil {
		t.Error("an SLA due before the case was opened must be rejected")
	}
	if _, err := engine.Create(ctx, caseDef, map[string]any{
		"case_number": "CASE-BAD2", "subject": "Bad priority", "customer_id": customer.ID,
		"priority": "catastrophic", "opened_date": "2026-08-01", "status_id": newStatus,
	}, actor); err == nil {
		t.Error("an undeclared priority must be rejected by the enum")
	}
}

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

// TestCase_LifecycleThroughTheGuardedEngine drives the graph the way
// internal/api does — ValidateStatusTransition then Update — rather
// than asserting which StatusTransition rows exist. A row's presence
// proves the seed; only this proves the engine honours it, and the
// reopen edge is the module's longest design paragraph.
func TestCase_LifecycleThroughTheGuardedEngine(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"crm", Publish},
		{"crm statuses", PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}
	engine := crud.NewEngine(tenantDB)
	caseDef := publishedDef(t, tenantDB, "Case")
	types, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "case_status")
	rows, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", types[0].ID)
	status := map[string]string{}
	for _, r := range rows {
		if c, _ := r.Data["code"].(string); c != "" {
			status[c] = r.ID
		}
	}
	customer, _ := engine.Create(ctx, publishedDef(t, tenantDB, "Party"), map[string]any{
		"party_type": "organization", "name": "Demo Customer", "status": "active",
	}, actor)

	fields := func(code string) map[string]any {
		return map[string]any{
			"case_number": "CASE-LC", "subject": "Lifecycle", "customer_id": customer.ID,
			"priority": "normal", "opened_date": "2026-08-01", "status_id": status[code],
		}
	}
	rec, err := engine.Create(ctx, caseDef, fields("new"), actor)
	if err != nil {
		t.Fatalf("create Case: %v", err)
	}
	version := rec.Version
	// The full journey an agent actually walks, including a stall on the
	// customer and a reopen.
	for _, to := range []string{"in_progress", "waiting_customer", "in_progress", "resolved", "in_progress", "resolved", "closed"} {
		if err := engine.ValidateStatusTransition(ctx, caseDef, rec.ID, fields(to), false, &version); err != nil {
			t.Fatalf("transition to %s should be legal: %v", to, err)
		}
		v, err := engine.Update(ctx, caseDef, rec.ID, fields(to), &version, actor)
		if err != nil {
			t.Fatalf("update to %s: %v", to, err)
		}
		version = v
	}
	// Closed is the end of the road.
	for _, to := range []string{"in_progress", "cancelled"} {
		if err := engine.ValidateStatusTransition(ctx, caseDef, rec.ID, fields(to), false, &version); err == nil {
			t.Errorf("closed -> %s must be rejected; reopening a closed case is a new case", to)
		}
	}

	// A case resolved and then found to be a duplicate must be
	// cancellable without faking a reopen — the edge the review found
	// missing, which would otherwise have corrupted resolution-time
	// measures.
	dup, err := engine.Create(ctx, caseDef, map[string]any{
		"case_number": "CASE-DUP", "subject": "Duplicate", "customer_id": customer.ID,
		"priority": "normal", "opened_date": "2026-08-01", "status_id": status["new"],
	}, actor)
	if err != nil {
		t.Fatalf("create duplicate Case: %v", err)
	}
	dupVersion := dup.Version
	dupFields := func(code string) map[string]any {
		return map[string]any{
			"case_number": "CASE-DUP", "subject": "Duplicate", "customer_id": customer.ID,
			"priority": "normal", "opened_date": "2026-08-01", "status_id": status[code],
		}
	}
	for _, to := range []string{"in_progress", "resolved"} {
		v, err := engine.Update(ctx, caseDef, dup.ID, dupFields(to), &dupVersion, actor)
		if err != nil {
			t.Fatalf("update to %s: %v", to, err)
		}
		dupVersion = v
	}
	if err := engine.ValidateStatusTransition(ctx, caseDef, dup.ID, dupFields("cancelled"), false, &dupVersion); err != nil {
		t.Errorf("a resolved case found to be a duplicate must be cancellable directly: %v", err)
	}
}
