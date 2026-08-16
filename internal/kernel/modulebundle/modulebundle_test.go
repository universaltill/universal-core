package modulebundle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
	"github.com/universaltill/universal-core/internal/kernel/moduleseed"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

func libraryBundle(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "library_bundle.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

// mutate round-trips the fixture through a generic map, applies fn, and
// re-marshals — the cheapest way to produce structurally-broken
// variants without maintaining a second fixture per error case.
func mutate(t *testing.T, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(libraryBundle(t), &m); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	fn(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal fixture: %v", err)
	}
	return out
}

// errGetVersionRepo is a minimal moduleseed.Repo stub whose GetVersion
// always fails — the only method blockedItems calls, and the one
// branch no DB-backed test can reach cheaply: a registry read failing
// right after PublishAll already succeeded (independent review,
// uc-infra#73). CreateDraft/Approve/Publish are never called by
// blockedItems, so they panic if that assumption ever stops holding.
type errGetVersionRepo struct{ err error }

func (r errGetVersionRepo) GetVersion(ctx context.Context, key string, version int) (data.DefinitionVersion, error) {
	return data.DefinitionVersion{}, r.err
}

func (r errGetVersionRepo) CreateDraft(ctx context.Context, key string, version int, definition []byte, actor audit.Actor) (data.DefinitionVersion, error) {
	panic("errGetVersionRepo: blockedItems must never call CreateDraft")
}

func (r errGetVersionRepo) Approve(ctx context.Context, key string, version int, actor audit.Actor) error {
	panic("errGetVersionRepo: blockedItems must never call Approve")
}

func (r errGetVersionRepo) Publish(ctx context.Context, key string, version int, actor audit.Actor) error {
	panic("errGetVersionRepo: blockedItems must never call Publish")
}

func TestBlockedItems_PropagatesGetVersionError(t *testing.T) {
	wantErr := errors.New("boom: registry unreachable")
	items := []moduleseed.Item{{Key: "Widget", Version: 1}}
	_, err := blockedItems(context.Background(), errGetVersionRepo{err: wantErr}, "entity", items)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("expected blockedItems to wrap and propagate the GetVersion error, got %v", err)
	}
}

func TestLoad_LibraryFixture(t *testing.T) {
	b, err := Load(libraryBundle(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if b.Module != "library" || b.Name != "Library" {
		t.Errorf("manifest wrong: %+v", b)
	}
	if len(b.Entities) != 3 || len(b.Forms) != 2 || len(b.StatusGraphs) != 1 {
		t.Fatalf("counts wrong: %d entities, %d forms, %d graphs", len(b.Entities), len(b.Forms), len(b.StatusGraphs))
	}
	var loan *entity.Definition
	for _, e := range b.Entities {
		if e.EntityType == "LibraryLoan" {
			loan = e
		}
	}
	if loan == nil || loan.StatusTypeCode != "library_loan_status" || len(loan.Relationships) != 1 {
		t.Errorf("LibraryLoan parsed wrong: %+v", loan)
	}
}

func TestLoad_Rejections(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  []byte
		want string
	}{
		"not json": {[]byte("{"), "parse bundle"},
		"unknown top-level field": {
			mutate(t, func(m map[string]any) { m["extra"] = true }), "parse bundle",
		},
		"wrong format": {
			mutate(t, func(m map[string]any) { m["format"] = "erp_module/v2" }), "unsupported bundle format",
		},
		"bad module key": {
			mutate(t, func(m map[string]any) { m["module"] = "My Library" }), "module key",
		},
		"no entities": {
			mutate(t, func(m map[string]any) { m["entities"] = []any{} }), "no entities",
		},
		"entity module mismatch": {
			mutate(t, func(m map[string]any) {
				m["entities"].([]any)[0].(map[string]any)["module"] = "other"
			}), "declares module",
		},
		"invalid entity definition": {
			mutate(t, func(m map[string]any) {
				m["entities"].([]any)[0].(map[string]any)["entity_type"] = ""
			}), "invalid entity definition",
		},
		// #71: entity.Validate now has a field-type allowlist, so a
		// hand-authored bundle (ADR-0012's own external-input path)
		// declaring a misspelled/unknown field type is rejected here too,
		// not just via the entity package's own unit tests.
		"unknown field type inside an entity definition": {
			mutate(t, func(m map[string]any) {
				fields := m["entities"].([]any)[0].(map[string]any)["fields"].([]any)
				fields[0].(map[string]any)["type"] = "isbn_number"
			}), "invalid entity definition",
		},
		"duplicate entity": {
			mutate(t, func(m map[string]any) {
				ents := m["entities"].([]any)
				m["entities"] = append(ents, ents[0])
			}), "duplicate entity",
		},
		"form without entity": {
			mutate(t, func(m map[string]any) {
				m["forms"].([]any)[0].(map[string]any)["entity_type"] = "NotInBundle"
			}), "no matching entity",
		},
		"status graph code mismatch": {
			mutate(t, func(m map[string]any) {
				m["status_graphs"].([]any)[0].(map[string]any)["entity_type"] = "LibraryBook"
			}), "does not match entity",
		},
		"status graph for entity not in bundle": {
			mutate(t, func(m map[string]any) {
				m["status_graphs"].([]any)[0].(map[string]any)["entity_type"] = "NotInBundle"
			}), "which is not in the bundle",
		},
		"reserved module key": {
			mutate(t, func(m map[string]any) {
				m["module"] = "foundation"
				for _, e := range m["entities"].([]any) {
					e.(map[string]any)["module"] = "foundation"
				}
			}), "reserved for a built-in module",
		},
		"entity with status_type_code but no graph": {
			mutate(t, func(m map[string]any) { delete(m, "status_graphs") }),
			"no matching status graph",
		},
		"duplicate status_type_code": {
			mutate(t, func(m map[string]any) {
				graphs := m["status_graphs"].([]any)
				dup := map[string]any{}
				for k, v := range graphs[0].(map[string]any) {
					dup[k] = v
				}
				// A second entity carrying the same code, so the
				// per-graph entity/code checks pass and the duplicate
				// check is what fires.
				ents := m["entities"].([]any)
				second := map[string]any{
					"entity_type": "LibraryHold", "version": float64(1), "module": "library",
					"status_type_code": "library_loan_status",
					"fields": []any{
						map[string]any{"name": "hold_number", "type": "string", "required": true},
						map[string]any{"name": "status_id", "type": "reference", "required": true, "target": "Status"},
					},
				}
				m["entities"] = append(ents, second)
				dup["entity_type"] = "LibraryHold"
				m["status_graphs"] = append(graphs, dup)
			}), "duplicate status_type_code",
		},
		"unknown field inside an entity definition": {
			mutate(t, func(m map[string]any) {
				fields := m["entities"].([]any)[0].(map[string]any)["fields"].([]any)
				fields[0].(map[string]any)["requred"] = true // typo for "required"
			}), "unknown field",
		},
		"unknown field inside a form definition": {
			mutate(t, func(m map[string]any) {
				m["forms"].([]any)[0].(map[string]any)["titel"] = "typo"
			}), "unknown field",
		},
		"duplicate form": {
			mutate(t, func(m map[string]any) {
				forms := m["forms"].([]any)
				m["forms"] = append(forms, forms[0])
			}), "duplicate form",
		},
		"empty status code": {
			mutate(t, func(m map[string]any) {
				m["status_graphs"].([]any)[0].(map[string]any)["statuses"].([]any)[1].(map[string]any)["code"] = ""
			}), "empty code",
		},
		"duplicate status code": {
			mutate(t, func(m map[string]any) {
				statuses := m["status_graphs"].([]any)[0].(map[string]any)["statuses"].([]any)
				statuses[1].(map[string]any)["code"] = "draft"
			}), "duplicates status code",
		},
		"no statuses": {
			mutate(t, func(m map[string]any) {
				m["status_graphs"].([]any)[0].(map[string]any)["statuses"] = []any{}
			}), "no statuses",
		},
		"trailing content after the manifest": {
			append(libraryBundle(t), []byte("{}")...), "trailing content",
		},
		"two initial statuses": {
			mutate(t, func(m map[string]any) {
				statuses := m["status_graphs"].([]any)[0].(map[string]any)["statuses"].([]any)
				statuses[1].(map[string]any)["is_initial"] = true
			}), "exactly one initial",
		},
		"transition to undeclared status": {
			mutate(t, func(m map[string]any) {
				trs := m["status_graphs"].([]any)[0].(map[string]any)["transitions"].([]any)
				trs[0].(map[string]any)["to"] = "vanished"
			}), "undeclared status",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(tc.raw)
			if err == nil {
				t.Fatalf("Load accepted an invalid bundle (%s)", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err.Error(), tc.want)
			}
		})
	}
}

// ---- integration (real Postgres) ----------------------------------------

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

	name := fmt.Sprintf("uc_test_modulebundle_%d", time.Now().UnixNano())
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
	return audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
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

// TestInstall_LibraryEndToEnd is the acceptance-criteria proof: install
// the fixture into a fresh tenant with foundation published, then use
// the module like any built-in one — definitions published, status
// graph seeded, a real record created through the guarded engine with
// a status transition validated against the seeded graph.
func TestInstall_LibraryEndToEnd(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}

	b, err := Load(libraryBundle(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	blocked, err := Install(ctx, tenantDB, b, actor)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(blocked) != 0 {
		t.Fatalf("expected no blocked items on a fresh install, got %+v", blocked)
	}

	// Registry state: every bundled definition published.
	for _, et := range []string{"LibraryBook", "LibraryLoan", "LibraryLoanLine"} {
		if got := publishedDef(t, tenantDB, et); got.Module != "library" {
			t.Errorf("%s published with module %q", et, got.Module)
		}
	}
	if _, err := data.NewFormDefinitionRepo(tenantDB).GetPublished(ctx, "LibraryLoan"); err != nil {
		t.Errorf("LibraryLoan form not published: %v", err)
	}

	// Status graph: rows exist and the engine enforces the graph.
	engine := crud.NewEngine(tenantDB)
	statusTypes, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "library_loan_status")
	if err != nil || len(statusTypes) != 1 {
		t.Fatalf("library_loan_status StatusType: err=%v n=%d", err, len(statusTypes))
	}
	statuses, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", statusTypes[0].ID)
	if err != nil || len(statuses) != 4 {
		t.Fatalf("statuses: err=%v n=%d", err, len(statuses))
	}
	statusID := map[string]string{}
	for _, s := range statuses {
		if c, _ := s.Data["code"].(string); c != "" {
			statusID[c] = s.ID
		}
	}

	// Use the module for real: borrower Party, a book, a draft loan.
	party, err := engine.Create(ctx, publishedDef(t, tenantDB, "Party"), map[string]any{
		"party_type": "person", "name": "Demo Borrower", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	book, err := engine.Create(ctx, publishedDef(t, tenantDB, "LibraryBook"), map[string]any{
		"isbn": "978-0000000001", "title": map[string]any{"en": "Demo Book"}, "copies": 3.0,
	}, actor)
	if err != nil {
		t.Fatalf("create LibraryBook: %v", err)
	}
	loanDef := publishedDef(t, tenantDB, "LibraryLoan")
	loan, err := engine.Create(ctx, loanDef, map[string]any{
		"loan_number": "L-1", "borrower_id": party.ID, "loan_date": "2026-07-31",
		"status_id": statusID["draft"],
	}, actor)
	if err != nil {
		t.Fatalf("create LibraryLoan: %v", err)
	}
	if _, err := engine.Create(ctx, publishedDef(t, tenantDB, "LibraryLoanLine"), map[string]any{
		"library_loan_id": loan.ID, "book_id": book.ID, "qty": 1.0,
	}, actor); err != nil {
		t.Fatalf("create LibraryLoanLine: %v", err)
	}

	// The seeded graph is enforced the way internal/api enforces it: an
	// explicit ValidateStatusTransition alongside Update (see that
	// method's doc comment for why it isn't folded into Update). draft
	// -> out is a declared edge, draft -> returned is not.
	loanFields := func(statusID string) map[string]any {
		return map[string]any{
			"loan_number": "L-1", "borrower_id": party.ID, "loan_date": "2026-07-31",
			"status_id": statusID,
		}
	}
	version := loan.Version
	if err := engine.ValidateStatusTransition(ctx, loanDef, loan.ID, loanFields(statusID["returned"]), false, &version); err == nil {
		t.Errorf("draft->returned should be rejected by the seeded transition graph")
	}
	if err := engine.ValidateStatusTransition(ctx, loanDef, loan.ID, loanFields(statusID["out"]), false, &version); err != nil {
		t.Errorf("draft->out should be legal: %v", err)
	} else if _, err := engine.Update(ctx, loanDef, loan.ID, loanFields(statusID["out"]), &version, actor); err != nil {
		t.Errorf("update to out: %v", err)
	}
}

// TestInstall_ReportsRolledBackVersionAsBlocked is the regression test
// for uc-infra#73: before this fix, Install returned a bare nil error
// in this exact scenario — moduleseed.PublishAll correctly leaves a
// rolled-back version alone (that part is deliberate and separately
// tested), but Install itself had no way to tell "already published"
// apart from "rolled back and skipped," so a re-install after an
// operator rollback reported unqualified success while the entity type
// stayed unpublished and unusable.
func TestInstall_ReportsRolledBackVersionAsBlocked(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	b, err := Load(libraryBundle(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Install(ctx, tenantDB, b, actor); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	entityRepo := data.NewEntityDefinitionRepo(tenantDB)
	formRepo := data.NewFormDefinitionRepo(tenantDB)
	if err := entityRepo.Rollback(ctx, "LibraryLoan", 1, actor); err != nil {
		t.Fatalf("Rollback LibraryLoan entity v1: %v", err)
	}
	if err := formRepo.Rollback(ctx, "LibraryLoan", 1, actor); err != nil {
		t.Fatalf("Rollback LibraryLoan form v1: %v", err)
	}

	blocked, err := Install(ctx, tenantDB, b, actor)
	if err != nil {
		t.Fatalf("re-Install should still succeed (blocked, not failed): %v", err)
	}
	want := []BlockedItem{
		{Kind: "entity", EntityType: "LibraryLoan", Version: 1},
		{Kind: "form", EntityType: "LibraryLoan", Version: 1},
	}
	if len(blocked) != len(want) {
		t.Fatalf("expected %d blocked item(s), got %d: %+v", len(want), len(blocked), blocked)
	}
	for _, w := range want {
		found := false
		for _, got := range blocked {
			if got == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %+v in blocked list, got %+v", w, blocked)
		}
	}

	// The entity type itself never comes back as published — this is
	// the actual user-visible symptom the issue describes: nothing of
	// this type can be created, despite Install having reported (before
	// this fix) an unqualified success.
	if _, err := entityRepo.GetPublished(ctx, "LibraryLoan"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected LibraryLoan entity to have no published version after rollback + re-install, got err=%v", err)
	}
	if _, err := formRepo.GetPublished(ctx, "LibraryLoan"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected LibraryLoan form to have no published version after rollback + re-install, got err=%v", err)
	}

	// Everything else in the same bundle that was NOT rolled back must
	// still be reported as fully installed, not swept into "blocked" —
	// this is a partial, per-item report, not an all-or-nothing flag.
	for _, et := range []string{"LibraryBook", "LibraryLoanLine"} {
		if _, err := entityRepo.GetPublished(ctx, et); err != nil {
			t.Errorf("%s should still be published (unaffected by LibraryLoan's rollback): %v", et, err)
		}
	}
}

func TestInstall_IdempotentReinstall(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	b, err := Load(libraryBundle(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Install(ctx, tenantDB, b, actor); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	blocked, err := Install(ctx, tenantDB, b, actor)
	if err != nil {
		t.Fatalf("re-Install should be a no-op success: %v", err)
	}
	if len(blocked) != 0 {
		t.Fatalf("re-install of an untouched bundle should report nothing blocked, got %+v", blocked)
	}
	// No duplicate status rows from the re-run.
	engine := crud.NewEngine(tenantDB)
	statusTypes, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "library_loan_status")
	if err != nil || len(statusTypes) != 1 {
		t.Fatalf("expected exactly one StatusType after re-install: err=%v n=%d", err, len(statusTypes))
	}
}

func TestInstall_RefusesDivergentContent(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	b, err := Load(libraryBundle(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Install(ctx, tenantDB, b, actor); err != nil {
		t.Fatalf("Install: %v", err)
	}

	diverged, err := Load(mutate(t, func(m map[string]any) {
		fields := m["entities"].([]any)[0].(map[string]any)["fields"].([]any)
		m["entities"].([]any)[0].(map[string]any)["fields"] = append(fields, map[string]any{
			"name": "shelf", "type": "string",
		})
	}))
	if err != nil {
		t.Fatalf("Load diverged: %v", err)
	}
	_, err = Install(ctx, tenantDB, diverged, actor)
	if err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("expected divergence refusal, got %v", err)
	}
}

// TestInstall_RefusesForeignStatusTypeCode is the regression test for
// the independent review's status-graph blocker: statusgraph.Seed
// resolves a StatusType by CODE, so a bundle naming an existing code
// would have reused purchasing's own StatusType row and injected a
// draft->received edge into it — letting any PurchaseOrder skip
// submitted/approved, an approval bypass on a financial document,
// installed by a bundle that never mentions purchasing.
func TestInstall_RefusesForeignStatusTypeCode(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := purchasing.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishStatuses: %v", err)
	}

	engine := crud.NewEngine(tenantDB)
	transitionDef := publishedDef(t, tenantDB, "StatusTransition")
	before, err := engine.List(ctx, transitionDef)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}

	hijack, err := Load(mutate(t, func(m map[string]any) {
		// One entity carrying purchasing's status type code, plus the
		// matching graph — a bundle that is internally consistent and
		// still must be refused.
		m["entities"] = []any{map[string]any{
			"entity_type": "LibraryThing", "version": float64(1), "module": "library",
			"status_type_code": "purchase_order_status",
			"fields": []any{
				map[string]any{"name": "code", "type": "string", "required": true},
				map[string]any{"name": "status_id", "type": "reference", "required": true, "target": "Status"},
			},
		}}
		delete(m, "forms")
		m["status_graphs"] = []any{map[string]any{
			"entity_type": "LibraryThing", "status_type_code": "purchase_order_status",
			"status_type_name": "Hijacked", "statuses": []any{
				map[string]any{"code": "draft", "name": "Draft", "sequence": float64(1), "is_initial": true, "is_terminal": false},
				map[string]any{"code": "received", "name": "Received", "sequence": float64(2), "is_initial": false, "is_terminal": true},
			},
			"transitions": []any{map[string]any{"from": "draft", "to": "received"}},
		}}
	}))
	if err != nil {
		t.Fatalf("Load hijack bundle: %v", err)
	}

	_, err = Install(ctx, tenantDB, hijack, actor)
	if err == nil || !strings.Contains(err.Error(), "already owned by entity") {
		t.Fatalf("expected status-type ownership refusal, got %v", err)
	}

	after, err := engine.List(ctx, transitionDef)
	if err != nil {
		t.Fatalf("list transitions after: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("purchasing's status graph was modified: %d -> %d transitions", len(before), len(after))
	}
	if _, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(ctx, "LibraryThing"); err == nil {
		t.Error("a refused bundle must not have published any definition")
	}
}

// A bundle may re-install its OWN status graph — the ownership check
// keys on the owning entity type, not merely on the code existing.
func TestInstall_ReinstallKeepsOwnStatusGraph(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	b, err := Load(libraryBundle(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Install(ctx, tenantDB, b, actor); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if _, err := Install(ctx, tenantDB, b, actor); err != nil {
		t.Fatalf("re-install must not trip its own status-type ownership check: %v", err)
	}
}

// TestInstall_StatusSpecTranslationsPassThrough is the plugin-shaped
// counterpart to uc-infra#244's built-in-module translations: a bundle
// (erp_module, ADR-0012) is CLAUDE.md's plugin-first "default new
// functionality to a plugin" route, so a bundle-defined status graph
// must be able to carry the same Translations content the six built-in
// Go modules' StatusSpecs() now do — otherwise a bundle-installed
// module's Status rows stay English-only forever, with no
// StatusSpecs()-driven backfill path able to help them either. Proves
// the JSON "translations" key survives Load -> Install and lands in the
// real seeded Status row's i18n_text "name", not just that the Go
// struct field exists.
func TestInstall_StatusSpecTranslationsPassThrough(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}

	translated, err := Load(mutate(t, func(m map[string]any) {
		statuses := m["status_graphs"].([]any)[0].(map[string]any)["statuses"].([]any)
		draft := statuses[0].(map[string]any)
		if draft["code"] != "draft" {
			t.Fatalf("test fixture assumption broken: expected statuses[0].code == \"draft\", got %v", draft["code"])
		}
		draft["translations"] = map[string]any{"tr": "Taslak", "ar": "مسودة"}
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Install(ctx, tenantDB, translated, actor); err != nil {
		t.Fatalf("Install: %v", err)
	}

	engine := crud.NewEngine(tenantDB)
	statusDef := publishedDef(t, tenantDB, "Status")
	recs, err := engine.ListByField(ctx, statusDef, "code", "draft")
	if err != nil {
		t.Fatalf("list Status by code=draft: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected exactly 1 seeded \"draft\" Status row, got %d", len(recs))
	}
	name, ok := recs[0].Data["name"].(map[string]any)
	if !ok {
		t.Fatalf("expected \"name\" as an i18n_text object, got %#v", recs[0].Data["name"])
	}
	if name["en"] != "Draft" || name["tr"] != "Taslak" || name["ar"] != "مسودة" {
		t.Fatalf(`expected {"en": "Draft", "tr": "Taslak", "ar": "مسودة"}, got %#v`, name)
	}
}

// Divergence conflicts are reported together, not one per attempt —
// the property ADR-0012 §5 promises.
func TestInstall_ReportsAllConflictsTogether(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	b, err := Load(libraryBundle(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Install(ctx, tenantDB, b, actor); err != nil {
		t.Fatalf("Install: %v", err)
	}
	diverged, err := Load(mutate(t, func(m map[string]any) {
		for _, e := range m["entities"].([]any) {
			ent := e.(map[string]any)
			ent["fields"] = append(ent["fields"].([]any), map[string]any{"name": "extra", "type": "string"})
		}
	}))
	if err != nil {
		t.Fatalf("Load diverged: %v", err)
	}
	_, err = Install(ctx, tenantDB, diverged, actor)
	if err == nil {
		t.Fatal("expected divergence refusal")
	}
	if got := strings.Count(err.Error(), "different content"); got != 3 {
		t.Errorf("expected all 3 conflicts in one error, got %d: %v", got, err)
	}
}

func TestInstall_RefusesForeignModuleEntityType(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}

	// A bundle claiming foundation's Party at a fresh version — a
	// hijack, not an upgrade, regardless of version novelty.
	hijack, err := Load(mutate(t, func(m map[string]any) {
		ent := m["entities"].([]any)[0].(map[string]any)
		ent["entity_type"] = "Party"
		ent["version"] = float64(99)
		m["entities"] = []any{ent}
		delete(m, "forms")
		delete(m, "status_graphs")
	}))
	if err != nil {
		t.Fatalf("Load hijack bundle: %v", err)
	}
	_, err = Install(ctx, tenantDB, hijack, actor)
	if err == nil || !strings.Contains(err.Error(), "owned by module") {
		t.Fatalf("expected foreign-module refusal, got %v", err)
	}
}
