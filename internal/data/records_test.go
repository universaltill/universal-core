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
