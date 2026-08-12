package data

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/db"
)

// freshTenantDB mirrors internal/kernel/crud's own fixture of the same
// name — RecordRepo's non-transactional Update/Delete wrappers
// (internal/kernel/crud.Engine always goes through UpdateTx/DeleteTx
// inside an explicit transaction, per CLAUDE.md's audit-atomicity rule,
// so these two thin wrappers had no coverage anywhere in the repo before
// this file) need the same real schema every other RecordRepo method is
// exercised against transitively through crud.Engine's own tests.
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

	name := fmt.Sprintf("uc_test_data_%d", time.Now().UnixNano())
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

// TestRecordRepo_UpdateUsesOwnConnectionPool confirms the non-Tx Update
// wrapper genuinely writes (via r.db, not a caller-supplied transaction)
// and delegates correctly to UpdateTx — same effective behavior, called
// without an explicit transaction.
func TestRecordRepo_UpdateUsesOwnConnectionPool(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	rec, err := repo.Create(ctx, "Widget", map[string]any{"name": "original"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	newVersion, err := repo.Update(ctx, "Widget", rec.ID, map[string]any{"name": "updated"}, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if newVersion != rec.Version+1 {
		t.Fatalf("expected version %d, got %d", rec.Version+1, newVersion)
	}

	got, err := repo.Get(ctx, "Widget", rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Data["name"] != "updated" {
		t.Fatalf("expected updated data to be persisted, got %+v", got.Data)
	}
}

// TestRecordRepo_UpdateVersionConflict confirms the non-Tx wrapper
// surfaces ErrVersionConflict exactly like UpdateTx does when
// expectedVersion is stale — the optimistic-locking behavior isn't lost
// by going through the thinner entry point.
func TestRecordRepo_UpdateVersionConflict(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	rec, err := repo.Create(ctx, "Widget", map[string]any{"name": "v1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stale := rec.Version - 1 // a version that will never match
	if _, err := repo.Update(ctx, "Widget", rec.ID, map[string]any{"name": "v2"}, &stale); err != ErrVersionConflict {
		t.Fatalf("expected ErrVersionConflict, got %v", err)
	}
}

// TestRecordRepo_UpdateNotFound confirms updating a nonexistent id
// through the non-Tx wrapper returns ErrNotFound, not ErrVersionConflict
// — UpdateTx's own doc comment explains why a single UPDATE can't tell
// these apart on its own and needs the follow-up existence check.
func TestRecordRepo_UpdateNotFound(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	if _, err := repo.Update(ctx, "Widget", "00000000-0000-0000-0000-000000000000", map[string]any{"name": "x"}, nil); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestRecordRepo_DeleteUsesOwnConnectionPool confirms the non-Tx Delete
// wrapper soft-deletes a real record (Get no longer finds it afterward)
// via its own connection pool.
func TestRecordRepo_DeleteUsesOwnConnectionPool(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	rec, err := repo.Create(ctx, "Widget", map[string]any{"name": "to-delete"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Delete(ctx, "Widget", rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, "Widget", rec.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestRecordRepo_DeleteNotFound(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	if err := repo.Delete(ctx, "Widget", "00000000-0000-0000-0000-000000000000"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRecordRepo_ListTx_ParticipatesInCallerTransaction(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	if _, err := repo.Create(ctx, "Widget", map[string]any{"name": "a"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	tx, err := tdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	got, err := repo.ListTx(ctx, tx, "Widget")
	if err != nil {
		t.Fatalf("ListTx: %v", err)
	}
	if len(got) != 1 || got[0].Data["name"] != "a" {
		t.Fatalf("expected 1 Widget record visible within the tx, got %+v", got)
	}
}

// TestRecordRepo_ListTx_SeesUncommittedWritesInSameTx confirms ListTx
// reads within the *same* transaction, not a separate connection —
// what internal/kernel/ledger's period check actually needs (seeing a
// Period this same transaction may have just touched, not only
// already-committed state a fresh connection would see).
func TestRecordRepo_ListTx_SeesUncommittedWritesInSameTx(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	tx, err := tdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := repo.CreateTx(ctx, tx, "Widget", map[string]any{"name": "uncommitted"}); err != nil {
		t.Fatalf("CreateTx: %v", err)
	}

	got, err := repo.ListTx(ctx, tx, "Widget")
	if err != nil {
		t.Fatalf("ListTx: %v", err)
	}
	if len(got) != 1 || got[0].Data["name"] != "uncommitted" {
		t.Fatalf("expected ListTx to see the uncommitted write within the same tx, got %+v", got)
	}

	// A separate connection must NOT see it (not committed yet) —
	// confirms this really is transaction-scoped visibility, not a
	// coincidence of List also happening to return the same row.
	outside, err := repo.List(ctx, "Widget")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(outside) != 0 {
		t.Fatalf("expected the uncommitted write to be invisible outside the tx, got %+v", outside)
	}
}

// ListPageFiltered/CountFiltered: sorting, substring filtering, and — the
// property that matters most for a JSONB query built from user input — no
// injection through the field name.
func TestRecordRepo_ListPageFilteredSortsAndFilters(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	// Values chosen to sort identically under ANY collation — all lower
	// case, no case-fold ambiguity — so this test isn't hostage to the
	// server's LC_COLLATE (a case assumption made the first version pass
	// locally and fail on CI's UTF-8-collated Postgres).
	for _, name := range []string{"cherry", "apple", "banana"} {
		if _, err := repo.Create(ctx, "Widget", map[string]any{"name": name}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	asc, err := repo.ListPageFiltered(ctx, "Widget", ListPageOptions{SortField: "name", Limit: 10})
	if err != nil {
		t.Fatalf("sort asc: %v", err)
	}
	got := []string{asc[0].Data["name"].(string), asc[1].Data["name"].(string), asc[2].Data["name"].(string)}
	if want := []string{"apple", "banana", "cherry"}; got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("ascending sort: got %v, want %v", got, want)
	}

	desc, err := repo.ListPageFiltered(ctx, "Widget", ListPageOptions{SortField: "name", SortDesc: true, Limit: 10})
	if err != nil {
		t.Fatalf("sort desc: %v", err)
	}
	if desc[0].Data["name"] != "cherry" {
		t.Fatalf("desc should start with cherry, got %v", desc[0].Data["name"])
	}

	// Substring filter, case-insensitive: only banana contains "an".
	filtered, err := repo.ListPageFiltered(ctx, "Widget", ListPageOptions{FilterField: "name", FilterValue: "AN", Limit: 10})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Data["name"] != "banana" {
		t.Fatalf("filter AN should match only banana (case-insensitively), got %v", filtered)
	}
	n, err := repo.CountFiltered(ctx, "Widget", ListPageOptions{FilterField: "name", FilterValue: "an"})
	if err != nil || n != 1 {
		t.Fatalf("CountFiltered an: got %d (%v), want 1", n, err)
	}
}

// A field name carrying SQL must not alter the query — it is bound to the
// ->> operator, never concatenated. Proven by using a hostile field name
// and confirming the table still exists and the query simply matches
// nothing.
func TestRecordRepo_ListPageFilteredFieldNameCannotInject(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)
	if _, err := repo.Create(ctx, "Widget", map[string]any{"name": "safe"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	hostile := "name'); DROP TABLE records; --"
	_, err := repo.ListPageFiltered(ctx, "Widget", ListPageOptions{SortField: hostile, Limit: 10})
	if err != nil {
		t.Fatalf("hostile sort field should be inert, not error: %v", err)
	}
	// The table must still be there.
	if _, err := repo.Create(ctx, "Widget", map[string]any{"name": "still here"}); err != nil {
		t.Fatalf("records table was harmed by a hostile field name: %v", err)
	}
}

// A numeric field must sort as a number, not as text — "100" after "90",
// not before it. This is wrong-list-on-an-accounting-product, not cosmetic.
func TestRecordRepo_ListPageFilteredNumericSort(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)
	for _, v := range []float64{100, 9, 90, 2} {
		if _, err := repo.Create(ctx, "Order", map[string]any{"total": v}); err != nil {
			t.Fatalf("create %v: %v", v, err)
		}
	}
	// Text sort would give 100, 2, 90, 9. Numeric must give 2, 9, 90, 100.
	got, err := repo.ListPageFiltered(ctx, "Order", ListPageOptions{SortField: "total", SortNumeric: true, Limit: 10})
	if err != nil {
		t.Fatalf("sort: %v", err)
	}
	var order []float64
	for _, r := range got {
		order = append(order, r.Data["total"].(float64))
	}
	want := []float64{2, 9, 90, 100}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("numeric sort wrong: got %v, want %v", order, want)
		}
	}
}

// A literal "%" or "_" in a filter must match itself, not act as a LIKE
// wildcard — otherwise typing "%" returns every row.
func TestRecordRepo_ListPageFilteredEscapesLikeWildcards(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)
	for _, n := range []string{"50%off", "plain", "under_score"} {
		if _, err := repo.Create(ctx, "Promo", map[string]any{"name": n}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	// "%" must match only the literal-percent row, not all three.
	pct, err := repo.ListPageFiltered(ctx, "Promo", ListPageOptions{FilterField: "name", FilterValue: "%", Limit: 10})
	if err != nil {
		t.Fatalf("filter %%: %v", err)
	}
	if len(pct) != 1 || pct[0].Data["name"] != "50%off" {
		t.Fatalf("literal %% should match only 50%%off, got %v", pct)
	}
	// "_" likewise.
	us, err := repo.ListPageFiltered(ctx, "Promo", ListPageOptions{FilterField: "name", FilterValue: "_", Limit: 10})
	if err != nil {
		t.Fatalf("filter _: %v", err)
	}
	if len(us) != 1 || us[0].Data["name"] != "under_score" {
		t.Fatalf("literal _ should match only under_score, got %v", us)
	}
}

// TestRecordRepo_ListPageFilteredEqualsFilters (uc-infra#78): EqualsFilters
// is an exact-match condition ANDed onto the query, independent of
// FilterField's own substring search — proves both that it filters at all
// and that it composes with FilterField rather than replacing it.
func TestRecordRepo_ListPageFilteredEqualsFilters(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p1", "role_type": "employee"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p2", "role_type": "vendor"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p3", "role_type": "employee"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	recs, err := repo.ListPageFiltered(ctx, "PersonRole", ListPageOptions{
		EqualsFilters: []FieldEquals{{Field: "role_type", Value: "employee"}},
		Limit:         10,
	})
	if err != nil {
		t.Fatalf("ListPageFiltered with EqualsFilters: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 employee rows, got %d: %+v", len(recs), recs)
	}

	n, err := repo.CountFiltered(ctx, "PersonRole", ListPageOptions{
		EqualsFilters: []FieldEquals{{Field: "role_type", Value: "employee"}},
	})
	if err != nil || n != 2 {
		t.Fatalf("CountFiltered with EqualsFilters: got %d (%v), want 2", n, err)
	}

	// Composes with a substring FilterField on a DIFFERENT field —
	// narrowing by role_type must not disable the picker's own free-text
	// search on person_id.
	both, err := repo.ListPageFiltered(ctx, "PersonRole", ListPageOptions{
		FilterField:   "person_id",
		FilterValue:   "p1",
		EqualsFilters: []FieldEquals{{Field: "role_type", Value: "employee"}},
		Limit:         10,
	})
	if err != nil {
		t.Fatalf("ListPageFiltered with FilterField+EqualsFilters: %v", err)
	}
	if len(both) != 1 || both[0].Data["person_id"] != "p1" {
		t.Fatalf("expected exactly p1, got %+v", both)
	}
}

// TestRecordRepo_ListPageFilteredIDIn (uc-infra#78): IDIn restricts to an
// explicit id set, independent of FilterField — the mechanism that lets a
// resolved TargetFilter role-holding join intersect with the picker's own
// label search.
func TestRecordRepo_ListPageFilteredIDIn(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	a, err := repo.Create(ctx, "Widget", map[string]any{"name": "alpha"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := repo.Create(ctx, "Widget", map[string]any{"name": "beta"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Create(ctx, "Widget", map[string]any{"name": "gamma"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	recs, err := repo.ListPageFiltered(ctx, "Widget", ListPageOptions{IDIn: []string{a.ID, b.ID}, Limit: 10})
	if err != nil {
		t.Fatalf("ListPageFiltered with IDIn: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 rows restricted by IDIn, got %d: %+v", len(recs), recs)
	}

	n, err := repo.CountFiltered(ctx, "Widget", ListPageOptions{IDIn: []string{a.ID}})
	if err != nil || n != 1 {
		t.Fatalf("CountFiltered with IDIn: got %d (%v), want 1", n, err)
	}

	// A non-nil EMPTY IDIn means "resolved to no matches" — must return
	// no rows, not "no restriction" (same convention FilterIn documents).
	empty, err := repo.ListPageFiltered(ctx, "Widget", ListPageOptions{IDIn: []string{}, Limit: 10})
	if err != nil {
		t.Fatalf("ListPageFiltered with empty IDIn: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected an empty (non-nil) IDIn to match nothing, got %+v", empty)
	}
}

// TestRecordRepo_ListPageFilteredJoinFilters (independent review,
// uc-infra#78 follow-up): JoinFilters is the correlated-EXISTS
// replacement for the original resolve-then-IDIn shape a TargetFilter
// entity-join condition used to go through (RecordRepo.DistinctFieldValues
// + ListPageOptions.IDIn) — this proves the SQL-pushed-down narrowing
// actually filters correctly end to end against real Postgres: only
// candidates with a matching row in the joined entity type come back,
// multiple JoinFilters entries AND together, and a soft-deleted joined
// row doesn't count as a match (same "deleted is absent" posture every
// other read in this file already takes).
func TestRecordRepo_ListPageFilteredJoinFilters(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	employee, err := repo.Create(ctx, "Person", map[string]any{"name": "Jamie"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": employee.ID, "role_type": "employee"}); err != nil {
		t.Fatalf("create role: %v", err)
	}
	vendor, err := repo.Create(ctx, "Person", map[string]any{"name": "Acme"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": vendor.ID, "role_type": "vendor"}); err != nil {
		t.Fatalf("create role: %v", err)
	}
	noRole, err := repo.Create(ctx, "Person", map[string]any{"name": "Nobody"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	jf := JoinFilter{Entity: "PersonRole", EntityField: "person_id", Field: "role_type", Value: "employee"}

	recs, err := repo.ListPageFiltered(ctx, "Person", ListPageOptions{JoinFilters: []JoinFilter{jf}, Limit: 10})
	if err != nil {
		t.Fatalf("ListPageFiltered with JoinFilters: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != employee.ID {
		t.Fatalf("expected exactly the employee record, got %+v", recs)
	}
	for _, rec := range recs {
		if rec.ID == vendor.ID || rec.ID == noRole.ID {
			t.Fatalf("JoinFilters leaked a non-matching candidate: %+v", recs)
		}
	}

	n, err := repo.CountFiltered(ctx, "Person", ListPageOptions{JoinFilters: []JoinFilter{jf}})
	if err != nil || n != 1 {
		t.Fatalf("CountFiltered with JoinFilters: got %d (%v), want 1", n, err)
	}

	// A soft-deleted PersonRole must not count as a match.
	deletedRoleHolder, err := repo.Create(ctx, "Person", map[string]any{"name": "Departed"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	deletedRole, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": deletedRoleHolder.ID, "role_type": "employee"})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := repo.Delete(ctx, "PersonRole", deletedRole.ID); err != nil {
		t.Fatalf("delete role: %v", err)
	}
	afterSoftDelete, err := repo.ListPageFiltered(ctx, "Person", ListPageOptions{JoinFilters: []JoinFilter{jf}, Limit: 10})
	if err != nil {
		t.Fatalf("ListPageFiltered with JoinFilters after soft-delete: %v", err)
	}
	for _, rec := range afterSoftDelete {
		if rec.ID == deletedRoleHolder.ID {
			t.Fatalf("a soft-deleted PersonRole should not satisfy the join filter, got %+v", afterSoftDelete)
		}
	}
}

// TestRecordRepo_ListPageFilteredJoinFilters_MultipleConditionsAND proves
// two JoinFilters entries on the same call compose with AND semantics —
// a candidate must satisfy EVERY entry, not any one of them (mirroring
// EqualsFilters' own AND composition, and replacing what intersectIDSets
// used to do in Go for the entity-join TargetFilter shape).
func TestRecordRepo_ListPageFilteredJoinFilters_MultipleConditionsAND(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	both, err := repo.Create(ctx, "Person", map[string]any{"name": "Both"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": both.ID, "role_type": "employee"}); err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := repo.Create(ctx, "PersonCert", map[string]any{"person_id": both.ID, "cert_type": "safety"}); err != nil {
		t.Fatalf("create cert: %v", err)
	}

	onlyEmployee, err := repo.Create(ctx, "Person", map[string]any{"name": "OnlyEmployee"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": onlyEmployee.ID, "role_type": "employee"}); err != nil {
		t.Fatalf("create role: %v", err)
	}

	recs, err := repo.ListPageFiltered(ctx, "Person", ListPageOptions{
		JoinFilters: []JoinFilter{
			{Entity: "PersonRole", EntityField: "person_id", Field: "role_type", Value: "employee"},
			{Entity: "PersonCert", EntityField: "person_id", Field: "cert_type", Value: "safety"},
		},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListPageFiltered with two JoinFilters: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != both.ID {
		t.Fatalf("expected exactly the record satisfying BOTH join filters, got %+v", recs)
	}
}

func TestRecordRepo_ExistsByFieldsQ(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p1", "role_type": "employee"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	exists, err := repo.ExistsByFieldsQ(ctx, db, "PersonRole", map[string]string{"person_id": "p1", "role_type": "employee"})
	if err != nil {
		t.Fatalf("ExistsByFieldsQ: %v", err)
	}
	if !exists {
		t.Fatal("expected a matching PersonRole to exist")
	}

	notEmployee, err := repo.ExistsByFieldsQ(ctx, db, "PersonRole", map[string]string{"person_id": "p1", "role_type": "vendor"})
	if err != nil {
		t.Fatalf("ExistsByFieldsQ: %v", err)
	}
	if notEmployee {
		t.Fatal("expected no PersonRole matching person_id=p1 AND role_type=vendor")
	}

	noSuchPerson, err := repo.ExistsByFieldsQ(ctx, db, "PersonRole", map[string]string{"person_id": "nobody", "role_type": "employee"})
	if err != nil {
		t.Fatalf("ExistsByFieldsQ: %v", err)
	}
	if noSuchPerson {
		t.Fatal("expected no PersonRole for a nonexistent person_id")
	}
}

func TestRecordRepo_ExistsByFieldsQ_RequiresAtLeastOnePair(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	if _, err := repo.ExistsByFieldsQ(ctx, db, "PersonRole", map[string]string{}); err == nil {
		t.Fatal("expected an error for zero field/value pairs, not a query with no filter at all")
	}
}

func TestRecordRepo_ExistsByFieldsQ_HostileFieldNameIsInert(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p1", "role_type": "employee"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	hostile := "role_type'); DROP TABLE records; --"
	if _, err := repo.ExistsByFieldsQ(ctx, db, "PersonRole", map[string]string{hostile: "employee"}); err != nil {
		t.Fatalf("hostile field name should be inert, not error: %v", err)
	}
	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p2", "role_type": "employee"}); err != nil {
		t.Fatalf("records table was harmed by a hostile field name: %v", err)
	}
}

func TestRecordRepo_GetByFieldsQ(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	created, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p1", "role_type": "employee"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rec, found, err := repo.GetByFieldsQ(ctx, db, "PersonRole", map[string]string{"person_id": "p1", "role_type": "employee"})
	if err != nil {
		t.Fatalf("GetByFieldsQ: %v", err)
	}
	if !found {
		t.Fatal("expected a matching PersonRole to be found")
	}
	if rec.ID != created.ID {
		t.Fatalf("GetByFieldsQ returned id %q, want %q", rec.ID, created.ID)
	}
	if rec.Version != created.Version {
		t.Fatalf("GetByFieldsQ returned version %d, want %d", rec.Version, created.Version)
	}
	if rec.Data["role_type"] != "employee" {
		t.Fatalf("GetByFieldsQ returned data %v, missing role_type", rec.Data)
	}
}

// TestRecordRepo_GetByFieldsQ_NotFoundIsNotAnError confirms the "no
// match" case surfaces as found=false, err=nil — not ErrNotFound — since
// an upsert's existence check treats "nothing here yet" as the ordinary
// create branch, not a failure to unwrap.
func TestRecordRepo_GetByFieldsQ_NotFoundIsNotAnError(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	rec, found, err := repo.GetByFieldsQ(ctx, db, "PersonRole", map[string]string{"person_id": "nobody", "role_type": "employee"})
	if err != nil {
		t.Fatalf("GetByFieldsQ: %v", err)
	}
	if found {
		t.Fatalf("expected no match, got %+v", rec)
	}
	if rec.ID != "" {
		t.Fatalf("expected a zero-value Record on no match, got id %q", rec.ID)
	}
}

// TestRecordRepo_GetByFieldsQ_PartialMatchIsNotFound confirms every
// named field/value pair must agree — matching on person_id alone while
// role_type disagrees must not return the record.
func TestRecordRepo_GetByFieldsQ_PartialMatchIsNotFound(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p1", "role_type": "employee"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, found, err := repo.GetByFieldsQ(ctx, db, "PersonRole", map[string]string{"person_id": "p1", "role_type": "vendor"})
	if err != nil {
		t.Fatalf("GetByFieldsQ: %v", err)
	}
	if found {
		t.Fatal("expected no match when one of two fields disagrees")
	}
}

// TestRecordRepo_GetByFieldsQ_ExcludesSoftDeleted confirms a soft-deleted
// record (deleted_at set) is invisible, same as every other live-record
// read in this file.
func TestRecordRepo_GetByFieldsQ_ExcludesSoftDeleted(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	created, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p1", "role_type": "employee"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Delete(ctx, "PersonRole", created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, found, err := repo.GetByFieldsQ(ctx, db, "PersonRole", map[string]string{"person_id": "p1", "role_type": "employee"})
	if err != nil {
		t.Fatalf("GetByFieldsQ: %v", err)
	}
	if found {
		t.Fatal("expected a soft-deleted record to be excluded")
	}
}

// TestRecordRepo_GetByFieldsQ_MultipleMatchesReturnsOldest confirms the
// deterministic "oldest wins" tie-break when more than one live record
// matches — mirrors crud.BackfillUniqueConstraintKeys' own convention
// for the same pre-existing-duplicate ambiguity.
func TestRecordRepo_GetByFieldsQ_MultipleMatchesReturnsOldest(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	first, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p1", "role_type": "employee"})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p1", "role_type": "employee"}); err != nil {
		t.Fatalf("create second: %v", err)
	}

	rec, found, err := repo.GetByFieldsQ(ctx, db, "PersonRole", map[string]string{"person_id": "p1", "role_type": "employee"})
	if err != nil {
		t.Fatalf("GetByFieldsQ: %v", err)
	}
	if !found {
		t.Fatal("expected a match")
	}
	if rec.ID != first.ID {
		t.Fatalf("GetByFieldsQ returned id %q, want the oldest record %q", rec.ID, first.ID)
	}
}

// TestRecordRepo_GetByFieldsQ_TiedCreatedAtIsStillDeterministic
// (independent review, uc-infra#126) is the regression test for a bug
// TestRecordRepo_GetByFieldsQ_MultipleMatchesReturnsOldest could not
// catch: that test's two rows come from separate implicit transactions,
// so their created_at values differ in practice even though nothing
// guarantees it. Two rows created inside the SAME transaction get the
// IDENTICAL created_at (Postgres' now() is transaction start time, not
// per-statement) — an ORDER BY created_at alone would then pick between
// them arbitrarily. This forces that exact tie and confirms the `id`
// tiebreak still returns a single, stable row across repeated calls.
func TestRecordRepo_GetByFieldsQ_TiedCreatedAtIsStillDeterministic(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	first, err := repo.CreateTx(ctx, tx, "PersonRole", map[string]any{"person_id": "p1", "role_type": "employee"})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := repo.CreateTx(ctx, tx, "PersonRole", map[string]any{"person_id": "p1", "role_type": "employee"})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var createdAtEqual bool
	if err := db.QueryRowContext(ctx,
		`SELECT (SELECT created_at FROM records WHERE id = $1) = (SELECT created_at FROM records WHERE id = $2)`,
		first.ID, second.ID,
	).Scan(&createdAtEqual); err != nil {
		t.Fatalf("compare created_at: %v", err)
	}
	if !createdAtEqual {
		t.Fatal("test setup assumption failed: the two rows' created_at values differ — this test no longer exercises the tie it's named for")
	}

	want := first.ID
	if second.ID < first.ID {
		want = second.ID
	}
	for i := 0; i < 5; i++ {
		rec, found, err := repo.GetByFieldsQ(ctx, db, "PersonRole", map[string]string{"person_id": "p1", "role_type": "employee"})
		if err != nil {
			t.Fatalf("GetByFieldsQ: %v", err)
		}
		if !found {
			t.Fatal("expected a match")
		}
		if rec.ID != want {
			t.Fatalf("GetByFieldsQ returned id %q on tied created_at, want the lexicographically-lower id %q (deterministic id tiebreak)", rec.ID, want)
		}
	}
}

func TestRecordRepo_GetByFieldsQ_RequiresAtLeastOnePair(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	if _, _, err := repo.GetByFieldsQ(ctx, db, "PersonRole", map[string]string{}); err == nil {
		t.Fatal("expected an error for zero field/value pairs, not a query with no filter at all")
	}
}

func TestRecordRepo_GetByFieldsQ_HostileFieldNameIsInert(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p1", "role_type": "employee"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	hostile := "role_type'); DROP TABLE records; --"
	if _, _, err := repo.GetByFieldsQ(ctx, db, "PersonRole", map[string]string{hostile: "employee"}); err != nil {
		t.Fatalf("hostile field name should be inert, not error: %v", err)
	}
	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p2", "role_type": "employee"}); err != nil {
		t.Fatalf("records table was harmed by a hostile field name: %v", err)
	}
}

func TestRecordRepo_DistinctFieldValues(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	for _, personID := range []string{"p1", "p2", "p1"} { // p1 holds the role twice, must dedupe
		if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": personID, "role_type": "employee"}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if _, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p3", "role_type": "vendor"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	ids, err := repo.DistinctFieldValues(ctx, "PersonRole", "role_type", "employee", "person_id")
	if err != nil {
		t.Fatalf("DistinctFieldValues: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if len(got) != 2 || !got["p1"] || !got["p2"] {
		t.Fatalf("expected exactly {p1, p2}, got %v", ids)
	}

	none, err := repo.DistinctFieldValues(ctx, "PersonRole", "role_type", "customer", "person_id")
	if err != nil {
		t.Fatalf("DistinctFieldValues: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected no matches for role_type=customer, got %v", none)
	}
}

func TestRecordRepo_DistinctFieldValues_ExcludesSoftDeletedRecords(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(db)

	rec, err := repo.Create(ctx, "PersonRole", map[string]any{"person_id": "p1", "role_type": "employee"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Delete(ctx, "PersonRole", rec.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	ids, err := repo.DistinctFieldValues(ctx, "PersonRole", "role_type", "employee", "person_id")
	if err != nil {
		t.Fatalf("DistinctFieldValues: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected a soft-deleted PersonRole to be excluded, got %v", ids)
	}
}

// TestRecordRepo_ListDueUnposted_FiltersDueAndUnposted is the direct
// regression test for uc-infra#182: assets.PostDueDepreciationBatch used
// to find its posting worklist by reading and decoding EVERY row of an
// entity type and filtering in Go; this asserts the query-level
// replacement selects exactly the rows that shape of code used to
// select — due (<= cutoff) and unposted (empty/absent/JSON-null) — and
// nothing else.
func TestRecordRepo_ListDueUnposted_FiltersDueAndUnposted(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	dueUnposted, err := repo.Create(ctx, "Task", map[string]any{"due_date": "2020-01-15", "done_at": ""})
	if err != nil {
		t.Fatalf("create dueUnposted: %v", err)
	}
	dueUnpostedAbsent, err := repo.Create(ctx, "Task", map[string]any{"due_date": "2020-01-16"})
	if err != nil {
		t.Fatalf("create dueUnpostedAbsent (no done_at field at all): %v", err)
	}
	dueUnpostedNull, err := repo.Create(ctx, "Task", map[string]any{"due_date": "2020-01-17", "done_at": nil})
	if err != nil {
		t.Fatalf("create dueUnpostedNull (JSON null done_at): %v", err)
	}
	// due_date exactly equal to cutoff — the boundary the <= comparison
	// must include, the single most common real case (an amount posts
	// ON its own due date, not strictly before it).
	dueOnCutoff, err := repo.Create(ctx, "Task", map[string]any{"due_date": "2020-06-01", "done_at": ""})
	if err != nil {
		t.Fatalf("create dueOnCutoff: %v", err)
	}
	duePosted, err := repo.Create(ctx, "Task", map[string]any{"due_date": "2020-01-15", "done_at": "2020-01-15"})
	if err != nil {
		t.Fatalf("create duePosted: %v", err)
	}
	future, err := repo.Create(ctx, "Task", map[string]any{"due_date": "2099-01-01", "done_at": ""})
	if err != nil {
		t.Fatalf("create future: %v", err)
	}
	noDueDate, err := repo.Create(ctx, "Task", map[string]any{"due_date": "", "done_at": ""})
	if err != nil {
		t.Fatalf("create noDueDate: %v", err)
	}
	otherType, err := repo.Create(ctx, "OtherTask", map[string]any{"due_date": "2020-01-15", "done_at": ""})
	if err != nil {
		t.Fatalf("create otherType: %v", err)
	}

	got, err := repo.ListDueUnposted(ctx, "Task", "due_date", "2020-06-01", "done_at", ListDueUnpostedGate{}, 100)
	if err != nil {
		t.Fatalf("ListDueUnposted: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, r := range got {
		gotIDs[r.ID] = true
	}
	want := map[string]bool{dueUnposted.ID: true, dueUnpostedAbsent.ID: true, dueUnpostedNull.ID: true, dueOnCutoff.ID: true}
	if len(gotIDs) != len(want) {
		t.Fatalf("got %d rows %v, want exactly %v", len(gotIDs), gotIDs, want)
	}
	for id := range want {
		if !gotIDs[id] {
			t.Errorf("expected due+unposted row %s in results, missing", id)
		}
	}
	for _, excluded := range []struct {
		name string
		id   string
	}{
		{"duePosted", duePosted.ID}, {"future", future.ID},
		{"noDueDate", noDueDate.ID}, {"otherType (wrong entity type)", otherType.ID},
	} {
		if gotIDs[excluded.id] {
			t.Errorf("%s (%s) should have been excluded, was returned", excluded.name, excluded.id)
		}
	}
}

// TestRecordRepo_ListDueUnposted_OrdersByCreatedAtThenIDAndRespectsLimit
// confirms the same (created_at, id) ordering and LIMIT behavior every
// other bounded/paginated query in this file already gives — which
// specific rows land in one capped call vs. the next must be
// deterministic, not whatever order Postgres happens to return.
func TestRecordRepo_ListDueUnposted_OrdersByCreatedAtThenIDAndRespectsLimit(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	var ids []string
	for i := 0; i < 5; i++ {
		rec, err := repo.Create(ctx, "Task", map[string]any{"due_date": "2020-01-01", "done_at": ""})
		if err != nil {
			t.Fatalf("create row %d: %v", i, err)
		}
		ids = append(ids, rec.ID)
	}

	got, err := repo.ListDueUnposted(ctx, "Task", "due_date", "2020-06-01", "done_at", ListDueUnpostedGate{}, 3)
	if err != nil {
		t.Fatalf("ListDueUnposted: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected exactly 3 rows (limit), got %d", len(got))
	}
	for i, r := range got {
		if r.ID != ids[i] {
			t.Errorf("row %d: got id %s, want %s (creation order)", i, r.ID, ids[i])
		}
	}
}

// TestRecordRepo_ListDueUnposted_ExcludesSoftDeletedRecords matches
// every other read method in this file's own deleted_at IS NULL scope.
func TestRecordRepo_ListDueUnposted_ExcludesSoftDeletedRecords(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	rec, err := repo.Create(ctx, "Task", map[string]any{"due_date": "2020-01-01", "done_at": ""})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Delete(ctx, "Task", rec.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	got, err := repo.ListDueUnposted(ctx, "Task", "due_date", "2020-06-01", "done_at", ListDueUnpostedGate{}, 100)
	if err != nil {
		t.Fatalf("ListDueUnposted: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected a soft-deleted row to be excluded, got %v", got)
	}
}

// TestRecordRepo_ListDueUnposted_GateExcludesRowsWithUnpostableParent is
// the direct regression test for independent review's finding on the
// first version of this method (uc-infra#182): a due-and-unposted row
// whose parent will NEVER become postable (wrong status, or missing
// required wiring) never mutates, so without the gate it would occupy
// the SAME LIMITed window on every future call forever — starving
// genuinely postable rows that happen to sort later by (created_at,
// id). This asserts the gate excludes exactly the unpostable rows, by
// construction, rather than relying on a caller-side budget to route
// around them (which the LIMIT itself defeats).
func TestRecordRepo_ListDueUnposted_GateExcludesRowsWithUnpostableParent(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	gate := ListDueUnpostedGate{
		ParentType: "Asset", JoinField: "asset_id",
		ParentMatchField: "status", ParentMatchValue: "active",
		ParentRequiredNonEmpty: []string{"account_a", "account_b"},
	}

	// Blocker 1: parent status wrong (disposed, not active).
	disposed, err := repo.Create(ctx, "Asset", map[string]any{"status": "disposed", "account_a": "x", "account_b": "y"})
	if err != nil {
		t.Fatalf("create disposed parent: %v", err)
	}
	blockedByStatus, err := repo.Create(ctx, "Schedule", map[string]any{"asset_id": disposed.ID, "due_date": "2020-01-01", "posted_at": ""})
	if err != nil {
		t.Fatalf("create blockedByStatus row: %v", err)
	}

	// Blocker 2: parent active but missing required wiring.
	unwired, err := repo.Create(ctx, "Asset", map[string]any{"status": "active", "account_a": ""})
	if err != nil {
		t.Fatalf("create unwired parent: %v", err)
	}
	blockedByWiring, err := repo.Create(ctx, "Schedule", map[string]any{"asset_id": unwired.ID, "due_date": "2020-01-02", "posted_at": ""})
	if err != nil {
		t.Fatalf("create blockedByWiring row: %v", err)
	}

	// Blocker 3: parent doesn't exist at all (empty/dangling reference).
	orphan, err := repo.Create(ctx, "Schedule", map[string]any{"asset_id": "", "due_date": "2020-01-03", "posted_at": ""})
	if err != nil {
		t.Fatalf("create orphan row: %v", err)
	}

	// Postable: active, fully wired — created LAST (latest created_at),
	// so a non-gated fetch ordered by (created_at, id) with a LIMIT
	// smaller than the blocker count would never even reach it.
	healthy, err := repo.Create(ctx, "Asset", map[string]any{"status": "active", "account_a": "x", "account_b": "y"})
	if err != nil {
		t.Fatalf("create healthy parent: %v", err)
	}
	postable, err := repo.Create(ctx, "Schedule", map[string]any{"asset_id": healthy.ID, "due_date": "2020-01-04", "posted_at": ""})
	if err != nil {
		t.Fatalf("create postable row: %v", err)
	}

	// The whole point: LIMIT 1, smaller than the 3 blockers ahead of it
	// in creation order. A gate-less fetch would return only a blocker
	// and never reach the postable row on ANY future call.
	got, err := repo.ListDueUnposted(ctx, "Schedule", "due_date", "2020-06-01", "posted_at", gate, 1)
	if err != nil {
		t.Fatalf("ListDueUnposted: %v", err)
	}
	if len(got) != 1 || got[0].ID != postable.ID {
		gotIDs := make([]string, len(got))
		for i, r := range got {
			gotIDs[i] = r.ID
		}
		t.Fatalf("got %v, want exactly [%s] (postable) — a blocked row must never occupy the LIMITed window", gotIDs, postable.ID)
	}

	// Sanity: a wide-open fetch (no gate) DOES return the blockers —
	// confirms the gate, not some other filter, is what's excluding them.
	ungated, err := repo.ListDueUnposted(ctx, "Schedule", "due_date", "2020-06-01", "posted_at", ListDueUnpostedGate{}, 100)
	if err != nil {
		t.Fatalf("ListDueUnposted (ungated): %v", err)
	}
	ungatedIDs := map[string]bool{}
	for _, r := range ungated {
		ungatedIDs[r.ID] = true
	}
	for _, id := range []string{blockedByStatus.ID, blockedByWiring.ID, orphan.ID, postable.ID} {
		if !ungatedIDs[id] {
			t.Errorf("ungated fetch should include %s, missing from %v", id, ungatedIDs)
		}
	}
}

// TestRecordRepo_LifeCompleteGroupIDs_FindsCompleteGroupsWithNoOutstandingDue
// is the direct regression test for uc-infra#182's completion/healing
// sweep: a parent whose children have met the quota (LifeField) AND
// have no outstanding due-and-unposted child is "life complete"; a
// parent short of quota, or one with an outstanding due child even
// after meeting quota, must not be.
func TestRecordRepo_LifeCompleteGroupIDs_FindsCompleteGroupsWithNoOutstandingDue(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	opts := func(matchValue string) LifeCompleteGroupOptions {
		return LifeCompleteGroupOptions{
			ParentType: "Parent", ParentMatchField: "status", ParentMatchValue: matchValue,
			LifeField: "quota", ChildType: "Child", ChildJoinField: "parent_id",
			ChildPostedField: "done_at", ChildDueField: "due_date", Cutoff: "2020-06-01",
		}
	}

	// complete: quota 2, both children done, nothing outstanding.
	complete, err := repo.Create(ctx, "Parent", map[string]any{"status": "active", "quota": float64(2)})
	if err != nil {
		t.Fatalf("create complete parent: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := repo.Create(ctx, "Child", map[string]any{"parent_id": complete.ID, "done_at": "2020-01-01"}); err != nil {
			t.Fatalf("create complete child %d: %v", i, err)
		}
	}

	// shortOfQuota: quota 3, only 2 children done.
	shortOfQuota, err := repo.Create(ctx, "Parent", map[string]any{"status": "active", "quota": float64(3)})
	if err != nil {
		t.Fatalf("create shortOfQuota parent: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := repo.Create(ctx, "Child", map[string]any{"parent_id": shortOfQuota.ID, "done_at": "2020-01-01"}); err != nil {
			t.Fatalf("create shortOfQuota child %d: %v", i, err)
		}
	}

	// quotaMetButStillDue: quota 1 already met, but a SEPARATE due,
	// unposted child still exists — must NOT be reported complete (this
	// is the exact uc-infra#137 premature-transition shape: don't
	// finalize while due work remains, regardless of the raw count).
	quotaMetButStillDue, err := repo.Create(ctx, "Parent", map[string]any{"status": "active", "quota": float64(1)})
	if err != nil {
		t.Fatalf("create quotaMetButStillDue parent: %v", err)
	}
	if _, err := repo.Create(ctx, "Child", map[string]any{"parent_id": quotaMetButStillDue.ID, "done_at": "2020-01-01"}); err != nil {
		t.Fatalf("create quotaMetButStillDue done child: %v", err)
	}
	if _, err := repo.Create(ctx, "Child", map[string]any{"parent_id": quotaMetButStillDue.ID, "due_date": "2020-01-02", "done_at": ""}); err != nil {
		t.Fatalf("create quotaMetButStillDue outstanding child: %v", err)
	}

	// futureChildOnly: quota met, and the only "outstanding" child isn't
	// due yet — must be reported complete (an unposted FUTURE row must
	// not block completion, same as the due-cutoff already means
	// elsewhere in this file).
	futureChildOnly, err := repo.Create(ctx, "Parent", map[string]any{"status": "active", "quota": float64(1)})
	if err != nil {
		t.Fatalf("create futureChildOnly parent: %v", err)
	}
	if _, err := repo.Create(ctx, "Child", map[string]any{"parent_id": futureChildOnly.ID, "done_at": "2020-01-01"}); err != nil {
		t.Fatalf("create futureChildOnly done child: %v", err)
	}
	if _, err := repo.Create(ctx, "Child", map[string]any{"parent_id": futureChildOnly.ID, "due_date": "2099-01-01", "done_at": ""}); err != nil {
		t.Fatalf("create futureChildOnly not-yet-due child: %v", err)
	}

	// wrongStatus: would otherwise be complete, but doesn't match
	// ParentMatchValue.
	wrongStatus, err := repo.Create(ctx, "Parent", map[string]any{"status": "archived", "quota": float64(1)})
	if err != nil {
		t.Fatalf("create wrongStatus parent: %v", err)
	}
	if _, err := repo.Create(ctx, "Child", map[string]any{"parent_id": wrongStatus.ID, "done_at": "2020-01-01"}); err != nil {
		t.Fatalf("create wrongStatus child: %v", err)
	}

	// zeroQuota: quota <= 0 must never read as "complete" regardless of
	// children — same ">0" gate assets.postAssetDepreciation's own
	// pre-uc-infra#182 useful_life_months check used.
	zeroQuota, err := repo.Create(ctx, "Parent", map[string]any{"status": "active", "quota": float64(0)})
	if err != nil {
		t.Fatalf("create zeroQuota parent: %v", err)
	}
	if _, err := repo.Create(ctx, "Child", map[string]any{"parent_id": zeroQuota.ID, "done_at": "2020-01-01"}); err != nil {
		t.Fatalf("create zeroQuota child: %v", err)
	}

	got, err := repo.LifeCompleteGroupIDs(ctx, opts("active"), 100)
	if err != nil {
		t.Fatalf("LifeCompleteGroupIDs: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, id := range got {
		gotIDs[id] = true
	}
	want := map[string]bool{complete.ID: true, futureChildOnly.ID: true}
	if len(gotIDs) != len(want) {
		t.Fatalf("got %v, want exactly %v", got, want)
	}
	for id := range want {
		if !gotIDs[id] {
			t.Errorf("expected parent %s to be reported life-complete, missing", id)
		}
	}
	for _, excluded := range []struct {
		name string
		id   string
	}{
		{"shortOfQuota", shortOfQuota.ID}, {"quotaMetButStillDue", quotaMetButStillDue.ID},
		{"wrongStatus", wrongStatus.ID}, {"zeroQuota", zeroQuota.ID},
	} {
		if gotIDs[excluded.id] {
			t.Errorf("%s (%s) should NOT have been reported life-complete", excluded.name, excluded.id)
		}
	}
}

// TestRecordRepo_LifeCompleteGroupIDs_RespectsLimit confirms the sweep
// is itself bounded and resumable across calls, the same "resumes on a
// later call" latency model ListDueUnposted's own cap already gives the
// posting side (uc-infra#182).
func TestRecordRepo_LifeCompleteGroupIDs_RespectsLimit(t *testing.T) {
	tdb := freshTenantDB(t)
	ctx := context.Background()
	repo := NewRecordRepo(tdb)

	for i := 0; i < 3; i++ {
		parent, err := repo.Create(ctx, "Parent", map[string]any{"status": "active", "quota": float64(1)})
		if err != nil {
			t.Fatalf("create parent %d: %v", i, err)
		}
		if _, err := repo.Create(ctx, "Child", map[string]any{"parent_id": parent.ID, "done_at": "2020-01-01"}); err != nil {
			t.Fatalf("create child %d: %v", i, err)
		}
	}

	got, err := repo.LifeCompleteGroupIDs(ctx, LifeCompleteGroupOptions{
		ParentType: "Parent", ParentMatchField: "status", ParentMatchValue: "active",
		LifeField: "quota", ChildType: "Child", ChildJoinField: "parent_id",
		ChildPostedField: "done_at", ChildDueField: "due_date", Cutoff: "2020-06-01",
	}, 2)
	if err != nil {
		t.Fatalf("LifeCompleteGroupIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 rows (limit), got %d: %v", len(got), got)
	}
}
