// Package data holds every repository — the only place raw SQL is allowed
// to live outside internal/db/migrations (CLAUDE.md, mirroring
// universal-till's enforced pattern).
package data

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrNotFound = errors.New("data: record not found")

// ErrVersionConflict is returned by UpdateTx/Update when the caller
// passed an expectedVersion that no longer matches the record's current
// version — someone else (or the same user, in another tab) saved a
// change since this caller last read the record. Distinct from
// ErrNotFound: the record is right there, just not at the version the
// caller thought it was.
var ErrVersionConflict = errors.New("data: record version conflict")

// Record is one row of the generic records table. Version is the
// optimistic-locking counter (starts at 1, incremented on every
// successful update) — see 005_record_version.sql's doc comment.
type Record struct {
	ID         string
	EntityType string
	Data       map[string]any
	Version    int
}

// querier is satisfied by both *sql.DB and *sql.Tx, so every repository
// method works standalone or inside a caller-managed transaction (needed
// so a record write and its audit entry commit atomically together).
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// RecordRepo is the repository for the generic entity-storage table.
// Every method takes a *sql.DB/querier already resolved to one tenant's
// own database (internal/tenantdb.Router) — tenant isolation is physical
// (ADR-0003), not a query-level filter, so no method here takes a
// tenantID.
type RecordRepo struct {
	db *sql.DB
}

func NewRecordRepo(db *sql.DB) *RecordRepo {
	return &RecordRepo{db: db}
}

// Create inserts a record using the repo's own connection pool (no
// caller-supplied transaction). Use CreateTx when the write must be
// atomic with another operation, such as an audit entry.
func (r *RecordRepo) Create(ctx context.Context, entityType string, data map[string]any) (Record, error) {
	return r.CreateTx(ctx, r.db, entityType, data)
}

func (r *RecordRepo) CreateTx(ctx context.Context, q querier, entityType string, data map[string]any) (Record, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Record{}, fmt.Errorf("marshal record data: %w", err)
	}
	var id string
	var version int
	err = q.QueryRowContext(ctx,
		`INSERT INTO records (entity_type, data)
		 VALUES ($1, $2)
		 RETURNING id, version`,
		entityType, raw,
	).Scan(&id, &version)
	if err != nil {
		return Record{}, fmt.Errorf("insert record: %w", err)
	}
	return Record{ID: id, EntityType: entityType, Data: data, Version: version}, nil
}

func (r *RecordRepo) Get(ctx context.Context, entityType, id string) (Record, error) {
	return r.get(ctx, r.db, entityType, id)
}

// GetTx is Get against a caller-supplied querier/tx instead of r.db —
// the same "same shape as the Tx write methods, now needed for a read
// too" reasoning CreateTx/UpdateTx/DeleteTx already established. First
// real caller: internal/kernel/crud's post-write hooks (ADR-0001's
// "lifecycle hooks" mechanism), which need to read another record (e.g.
// a GoodsReceiptLine's POLine) from within the same transaction as the
// write that triggered the hook, not a separate connection.
func (r *RecordRepo) GetTx(ctx context.Context, q querier, entityType, id string) (Record, error) {
	return r.get(ctx, q, entityType, id)
}

// GetTxIncludingDeleted is GetTx without the `deleted_at IS NULL` filter
// every other read applies — it sees a soft-deleted record's own stored
// data instead of treating it as absent. The one caller that needs this:
// crud.checkSelfReferenceCycle, which walks a self-referencing field's
// real stored reference chain (e.g. Account.parent_account_id) to detect
// a cycle. A soft-deleted record's data is still physically present, so
// a chain routed through one can still form a real cycle with a live
// record — GetTx's normal deleted-record-is-absent view would let that
// slip past undetected, reporting "reached the top of the chain" for a
// node that in fact still points right back into the cycle.
func (r *RecordRepo) GetTxIncludingDeleted(ctx context.Context, q querier, entityType, id string) (Record, error) {
	var raw []byte
	var version int
	err := q.QueryRowContext(ctx,
		`SELECT data, version FROM records WHERE id = $1 AND entity_type = $2`,
		id, entityType,
	).Scan(&raw, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("get record (including deleted): %w", err)
	}
	var recData map[string]any
	if err := json.Unmarshal(raw, &recData); err != nil {
		return Record{}, fmt.Errorf("unmarshal record data: %w", err)
	}
	return Record{ID: id, EntityType: entityType, Data: recData, Version: version}, nil
}

// LockSelfReferenceChainTx takes a per-entity-type Postgres advisory
// transaction lock (pg_advisory_xact_lock) — serializes every concurrent
// caller locking the same entityType within this tenant's database until
// each one's transaction commits or rolls back. crud.Engine.Update's own
// doc comment explains why a self-referencing FieldReference update
// needs this (two concurrent updates can each close one half of a cycle
// without ever observing the other's still-uncommitted write). Takes
// *sql.Tx specifically, not the generic querier interface most of this
// file's methods accept: pg_advisory_xact_lock only means what its name
// says inside a transaction — called against a bare connection it would
// hold for the rest of that connection's life instead of releasing at
// commit/rollback, so requiring *sql.Tx here makes that misuse a compile
// error rather than a runtime surprise.
func (r *RecordRepo) LockSelfReferenceChainTx(ctx context.Context, tx *sql.Tx, entityType string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "self-ref:"+entityType); err != nil {
		return fmt.Errorf("acquire self-reference lock for %s: %w", entityType, err)
	}
	return nil
}

func (r *RecordRepo) get(ctx context.Context, q querier, entityType, id string) (Record, error) {
	var raw []byte
	var version int
	err := q.QueryRowContext(ctx,
		`SELECT data, version FROM records
		 WHERE id = $1 AND entity_type = $2 AND deleted_at IS NULL`,
		id, entityType,
	).Scan(&raw, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("get record: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return Record{}, fmt.Errorf("unmarshal record data: %w", err)
	}
	return Record{ID: id, EntityType: entityType, Data: data, Version: version}, nil
}

func (r *RecordRepo) List(ctx context.Context, entityType string) ([]Record, error) {
	return r.list(ctx, r.db, entityType)
}

// ListTx is List against a caller-supplied querier/tx instead of r.db —
// same "same shape as the Tx write methods, now needed for a read too"
// reasoning GetTx's own doc comment gives. First real caller:
// internal/kernel/ledger.PostTx's period-close check, which needs to
// read every Period record from within the same transaction as the
// journal entry it's about to write (or refuse to write).
func (r *RecordRepo) ListTx(ctx context.Context, q querier, entityType string) ([]Record, error) {
	return r.list(ctx, q, entityType)
}

func (r *RecordRepo) list(ctx context.Context, q querier, entityType string) ([]Record, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, data, version FROM records
		 WHERE entity_type = $1 AND deleted_at IS NULL
		 ORDER BY created_at`,
		entityType,
	)
	if err != nil {
		return nil, fmt.Errorf("list records: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var id string
		var raw []byte
		var version int
		if err := rows.Scan(&id, &raw, &version); err != nil {
			return nil, fmt.Errorf("scan record: %w", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("unmarshal record data: %w", err)
		}
		out = append(out, Record{ID: id, EntityType: entityType, Data: data, Version: version})
	}
	return out, rows.Err()
}

// ListDueUnpostedGate optionally narrows RecordRepo.ListDueUnposted to
// rows whose parent record is currently in a state that makes the row
// actually POSTABLE, not merely due — see ListDueUnposted's own doc
// comment for why this matters once the worklist itself is LIMITed.
// Zero value (ParentType == "") means no gate is applied.
type ListDueUnpostedGate struct {
	ParentType string
	// JoinField is this (child) entity's field holding the parent
	// record's own id.
	JoinField        string
	ParentMatchField string // parent field to match, e.g. "status_id"
	ParentMatchValue string
	// ParentRequiredNonEmpty lists additional parent fields that must
	// all hold a non-empty value for a child row to be included.
	ParentRequiredNonEmpty []string
}

// ListDueUnposted returns up to limit non-deleted entityType records
// whose dueField holds a non-empty value <= cutoff AND whose
// postedField is empty (absent, JSON null, or "") — the bounded,
// query-level worklist assets.PostDueDepreciationBatch needs
// (entityType="DepreciationSchedule", dueField="period_end",
// postedField="posted_at") to find rows to post without reading and
// decoding the tenant's ENTIRE table for that entity type on every call
// (uc-infra#182: the previous shape was List(entityType) followed by
// in-Go filtering of every row). Ordered by (created_at, id) — the same
// stable, deterministic order every other paginated/limited query in
// this file uses — so which rows land in one capped call vs. the next
// is predictable.
//
// gate, when its ParentType is non-empty, additionally requires the
// row's parent (found via gate.JoinField) to currently match
// gate.ParentMatchField=gate.ParentMatchValue and have every field in
// gate.ParentRequiredNonEmpty non-empty. This exists to prevent a
// starvation bug independent review caught in this method's first
// version (uc-infra#182): a row that can never actually be posted — its
// parent isn't in the right state (e.g. a disposed FixedAsset, whose
// remaining schedule rows stay due-and-unposted forever by design — see
// assets.PostDueDepreciation's own doc comment) or is missing required
// wiring — never mutates, so without the gate it sorts into the SAME
// LIMITed window on every future call forever. Once enough such rows
// exist earlier in (created_at, id) order than any genuinely postable
// row, this worklist would return only permanently-stuck rows and
// posting would silently stop for the whole tenant. The gate excludes
// such rows from the worklist entirely, so they never occupy a slot —
// exactly mirroring what the caller's own per-row posting logic would
// have skipped anyway, just decided at the query level instead of after
// spending a worklist slot on it.
//
// This narrows, but does not claim to catch, every possible permanent-
// failure mode — an asset whose account fields are all non-empty but
// point at a since-deleted Account record (a narrower, unconfirmed
// hazard, not the concretely demonstrated one this gate fixes) is not
// covered; see ADR-0026's own note on this residual gap.
//
// dueField/postedField/gate fields are all bound as parameters to the
// ->> operator, never concatenated into the query text — same
// discipline every other generic field-name-driven query in this file
// follows; the CALLER is responsible for passing real field names.
//
// A dueField value of "" is deliberately excluded (data->>dueField <>
// ”), not merely compared <= cutoff: an empty string sorts before
// every real ISO date lexically, so without this a malformed row with
// no due date at all would incorrectly read as "due" — matching the
// in-Go filter this replaces (assets.postAssetDepreciation's own
// "periodEnd == "" ... not due yet" check).
func (r *RecordRepo) ListDueUnposted(ctx context.Context, entityType, dueField, cutoff, postedField string, gate ListDueUnpostedGate, limit int) ([]Record, error) {
	args := []any{entityType, postedField, dueField, cutoff}
	where := `entity_type = $1 AND deleted_at IS NULL
		   AND coalesce(data->>$2, '') = ''
		   AND data->>$3 <> '' AND data->>$3 <= $4`
	if gate.ParentType != "" {
		args = append(args, gate.ParentType, gate.JoinField, gate.ParentMatchField, gate.ParentMatchValue)
		n := len(args)
		join := fmt.Sprintf(
			`SELECT 1 FROM records parent
			 WHERE parent.entity_type = $%d AND parent.deleted_at IS NULL
			   AND parent.id::text = records.data->>$%d
			   AND parent.data->>$%d = $%d`,
			n-3, n-2, n-1, n,
		)
		for _, field := range gate.ParentRequiredNonEmpty {
			args = append(args, field)
			join += fmt.Sprintf(` AND coalesce(parent.data->>$%d, '') <> ''`, len(args))
		}
		where += " AND EXISTS (" + join + ")"
	}
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, data, version FROM records
		 WHERE %s
		 ORDER BY created_at, id
		 LIMIT $%d`, where, len(args)),
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("list due unposted records: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var id string
		var raw []byte
		var version int
		if err := rows.Scan(&id, &raw, &version); err != nil {
			return nil, fmt.Errorf("scan record: %w", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("unmarshal record data: %w", err)
		}
		out = append(out, Record{ID: id, EntityType: entityType, Data: data, Version: version})
	}
	return out, rows.Err()
}

// LifeCompleteGroupOptions parametrizes RecordRepo.LifeCompleteGroupIDs
// — see that method's own doc comment.
type LifeCompleteGroupOptions struct {
	ParentType       string
	ParentMatchField string // parent field to match, e.g. "status_id"
	ParentMatchValue string
	LifeField        string // parent field, numeric — the child "quota"
	ChildType        string
	ChildJoinField   string // child field holding the parent's own id
	ChildPostedField string // child field; non-empty means "counts toward the quota"
	ChildDueField    string // child field, an ISO date string
	Cutoff           string // ISO date; a child is "due" when ChildDueField <= Cutoff
	// ParentCounterField (uc-infra#202), optional: a parent field,
	// numeric, that the CALLER maintains as an approximate running count
	// of children satisfying ChildPostedField. When set,
	// LifeCompleteGroupIDs compares this stored value to LifeField
	// directly instead of re-deriving the count from a correlated
	// count(*) over ChildType — O(1) per candidate parent instead of
	// O(that parent's own child-row count) for the quota half of the
	// check specifically (the NOT EXISTS due-row half is identical
	// either way — see this method's own doc comment on why that half
	// isn't actually index-served, so this option speeds up the common
	// case, not every case). When empty (the default, and every caller
	// before this one), behavior is unchanged: the count(*) subquery
	// re-derives the true count from the child rows themselves every
	// call.
	//
	// TREAT THIS AS A FAST, SOMETIMES-WRONG FILTER, NOT AN AUTHORITY —
	// a caller-maintained counter can drift (low OR high) from whatever
	// out-of-band writes touch the child rows outside the caller's own
	// bookkeeping path (assets.FixedAsset.posted_period_count's own doc
	// comment has the concrete example: independent review found
	// DepreciationScheduleForm can change a child's ChildPostedField
	// value without the parent counter following). A result this option
	// includes must be re-verified against the real child rows before
	// anything irreversible happens to it — see
	// assets.verifyLifeComplete/healStuckFullyDepreciatedAssets for the
	// pattern this repo's own caller uses. A missing or non-numeric
	// ParentCounterField value on a given parent (e.g. a row written
	// before the caller's own Version bump added the field, ahead of a
	// backfill) makes that parent's quota comparison evaluate to
	// NULL/false, the same "not complete" fail-safe LifeField's own
	// `> 0` regex guard already gives a missing/malformed LifeField —
	// never a false "complete" from THIS half of the check (the
	// caller-side verification step is still what closes the loop).
	ParentCounterField string
}

// LifeCompleteGroupIDs returns up to limit ParentType record ids
// matching ParentMatchField=ParentMatchValue whose ChildType children
// (joined via ChildJoinField == the parent's own id) satisfy BOTH:
//   - at least LifeField-many (parsed as a positive number) children
//     have a non-empty ChildPostedField, AND
//   - no child has an EMPTY ChildPostedField and a ChildDueField <=
//     Cutoff (nothing due is still outstanding for that parent).
//
// First real caller: assets.PostDueDepreciationBatch's completion/
// healing sweep (uc-infra#182) — finds FixedAsset records that are
// currently in_service but whose entire DepreciationSchedule has
// already been posted, so the ordinary due-row worklist
// (ListDueUnposted above) — which only ever surfaces rows still
// needing posting — would never visit them again to run the completion
// check that transitions them out of in_service. See
// PostDueDepreciationBatch's own doc comment for the retry-safety
// invariant this preserves (uc-infra#137's "fire-once-or-never"
// finding): an asset that finishes its schedule but whose transition
// write never lands (a crash, a version conflict, an overlapping
// poller) has ZERO due-or-unposted rows left, so nothing about it looks
// like "due work" to the ordinary worklist — this sweep is what still
// finds it.
//
// Computed as one SQL query — a self-join on the same generic `records`
// table (parent and child rows both live there) using a correlated
// NOT EXISTS and (unless ParentCounterField is set — see below) a
// correlated count — rather than a Go-side loop reading and decoding
// every child row for every candidate parent. In the ordinary healthy
// steady state (nothing stuck), this is fast: don't assume that
// generalizes to "proportional to how many parents are ACTUALLY
// stuck" without checking ADR-0026/ADR-0029 first — independent review
// measured the NOT EXISTS anti-join itself is NOT index-served (no
// index on `records.data` can serve a bind-parameterized `->>` key),
// so a call where most candidates genuinely ARE stuck still costs
// close to the tenant's total child-row volume, `LIMIT` notwithstanding
// (`ORDER BY parent.id LIMIT $n` bounds the RESULT size, not the work —
// the sort has to consume every qualifying candidate before
// truncating). This is a real, currently-unfixed limitation of the
// underlying anti-join both before and after uc-infra#202's
// ParentCounterField addition — not something that option regresses.
//
// Every field name is bound as a parameter to the ->> operator, never
// concatenated into the query text, the same discipline every other
// generic field-name-driven query in this file follows.
func (r *RecordRepo) LifeCompleteGroupIDs(ctx context.Context, opts LifeCompleteGroupOptions, limit int) ([]string, error) {
	var rows *sql.Rows
	var err error
	if opts.ParentCounterField != "" {
		// Counter fast path (uc-infra#202): the quota check is a direct
		// comparison against a value the caller already maintains,
		// instead of a correlated count(*) over every candidate
		// parent's own child rows — see LifeCompleteGroupOptions.
		// ParentCounterField's own doc comment for why this result is a
		// filter for the caller to re-verify, not an authority. The NOT
		// EXISTS due-row check is IDENTICAL to the slow path's own below
		// — same query shape, same cost characteristics (this method's
		// own doc comment covers why that's NOT actually index-served
		// once the field name is a bind parameter rather than a literal;
		// don't call it "cheap" here without re-checking that first).
		rows, err = r.db.QueryContext(ctx, `
			SELECT parent.id
			FROM records parent
			WHERE parent.entity_type = $1
			  AND parent.deleted_at IS NULL
			  AND parent.data->>$2 = $3
			  AND parent.data->>$4 ~ '^-?[0-9]+(\.[0-9]+)?$'
			  AND (parent.data->>$4)::numeric > 0
			  AND parent.data->>$5 ~ '^-?[0-9]+(\.[0-9]+)?$'
			  AND (parent.data->>$5)::numeric >= (parent.data->>$4)::numeric
			  AND NOT EXISTS (
				SELECT 1 FROM records child
				WHERE child.entity_type = $6
				  AND child.deleted_at IS NULL
				  AND child.data->>$7 = parent.id::text
				  AND coalesce(child.data->>$8, '') = ''
				  AND child.data->>$9 <> '' AND child.data->>$9 <= $10
			  )
			ORDER BY parent.id
			LIMIT $11`,
			opts.ParentType, opts.ParentMatchField, opts.ParentMatchValue, opts.LifeField,
			opts.ParentCounterField,
			opts.ChildType, opts.ChildJoinField, opts.ChildPostedField, opts.ChildDueField, opts.Cutoff,
			limit,
		)
	} else {
		rows, err = r.db.QueryContext(ctx, `
			SELECT parent.id
			FROM records parent
			WHERE parent.entity_type = $1
			  AND parent.deleted_at IS NULL
			  AND parent.data->>$2 = $3
			  AND parent.data->>$4 ~ '^-?[0-9]+(\.[0-9]+)?$'
			  AND (parent.data->>$4)::numeric > 0
			  AND NOT EXISTS (
				SELECT 1 FROM records child
				WHERE child.entity_type = $5
				  AND child.deleted_at IS NULL
				  AND child.data->>$6 = parent.id::text
				  AND coalesce(child.data->>$7, '') = ''
				  AND child.data->>$8 <> '' AND child.data->>$8 <= $9
			  )
			  AND (
				SELECT count(*) FROM records child2
				WHERE child2.entity_type = $5
				  AND child2.deleted_at IS NULL
				  AND child2.data->>$6 = parent.id::text
				  AND coalesce(child2.data->>$7, '') <> ''
			  ) >= (parent.data->>$4)::numeric
			ORDER BY parent.id
			LIMIT $10`,
			opts.ParentType, opts.ParentMatchField, opts.ParentMatchValue, opts.LifeField,
			opts.ChildType, opts.ChildJoinField, opts.ChildPostedField, opts.ChildDueField, opts.Cutoff,
			limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("find life-complete groups: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan life-complete group id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CountByEntityType returns how many non-deleted records of entityType
// exist — the total a pager needs to compute page count, kept as its own
// query rather than folded into ListPage via a window function
// (count(*) OVER()) so the common "page beyond the last one" case (an
// empty LIMIT/OFFSET result set) doesn't lose the total along with the
// rows; two simple queries over one query with an edge case.
func (r *RecordRepo) CountByEntityType(ctx context.Context, entityType string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM records WHERE entity_type = $1 AND deleted_at IS NULL`,
		entityType,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count records: %w", err)
	}
	return n, nil
}

// ListPage returns one page of entityType records — the same ordering as
// List (created_at, with id as a tiebreaker so pagination stays
// deterministic even when two records share a created_at). Kept distinct
// from List rather than adding limit/offset params there: every existing
// List caller (the JSON API, reference-option dropdowns, master-detail
// child loading) genuinely wants every matching row, not a page of them
// — this is additive for the one caller that doesn't (the HTML list
// page).
func (r *RecordRepo) ListPage(ctx context.Context, entityType string, limit, offset int) ([]Record, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, data, version FROM records
		 WHERE entity_type = $1 AND deleted_at IS NULL
		 ORDER BY created_at, id
		 LIMIT $2 OFFSET $3`,
		entityType, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list records page: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var id string
		var raw []byte
		var version int
		if err := rows.Scan(&id, &raw, &version); err != nil {
			return nil, fmt.Errorf("scan record: %w", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("unmarshal record data: %w", err)
		}
		out = append(out, Record{ID: id, EntityType: entityType, Data: data, Version: version})
	}
	return out, rows.Err()
}

// ListPageOptions describes an optional sort and single-field filter for
// ListPageFiltered. Zero value == ListPage's behaviour (created_at order,
// no filter), so the plain paginated path is unchanged.
//
// SortField/FilterField are JSON keys inside the data blob. The CALLER
// (internal/api) must validate them against the entity Definition's real
// fields before passing them here — an unknown field is a caller bug, not
// a query-structure risk: both are bound as parameters to the ->>
// operator, never concatenated into the SQL, so even an unvalidated value
// cannot alter the query. Validation exists so a typo'd sort field is a
// clean "unknown field" rather than a silently-empty ORDER BY key.
type ListPageOptions struct {
	SortField string // "" = created_at (the stable default)
	SortDesc  bool
	// SortNumeric sorts the field as a number, not text — set by the
	// caller for FieldNumber fields so "10" sorts after "9". A JSON text
	// extraction sorts lexically otherwise, which is wrong for any amount
	// or quantity.
	SortNumeric bool
	FilterField string // "" = no filter
	FilterValue string // substring match (ILIKE), only used when FilterField set
	// FilterIn, when non-nil, replaces the substring match with an exact
	// membership test against these values. It exists for filtering a
	// FieldReference column: the stored value there is a target record's
	// id, so an ILIKE against what a human typed matches nothing (a user
	// searching an attendance list for "EMP-0001" got zero rows —
	// independent review, board #17). The caller resolves the typed text
	// to matching target ids and passes them here.
	//
	// An EMPTY non-nil slice means "resolved to no matches" and must
	// return no rows — distinct from nil, which means "not a reference
	// filter". Collapsing the two would turn a search with no hits into
	// a search with no filter, which is the more dangerous default.
	FilterIn []string
	// EqualsFilters are additional exact-match (data->>field = value)
	// conditions ANDed onto the query, independent of FilterField's own
	// substring search — the mechanism a FieldReference's declared
	// TargetFilter direct-field condition (Field.TargetFilter with
	// Entity == "") uses to narrow a reference picker's candidates
	// (uc-infra#78), alongside the picker's own free-text search on the
	// label field. Every field name is bound as a parameter, the same
	// discipline FilterField/SortField already follow.
	EqualsFilters []FieldEquals
	// IDIn optionally restricts results to only these ids — independent
	// of FilterField/FilterValue/FilterIn (which test a DECLARED
	// field's own value against the query). Nil means no restriction. A
	// non-nil EMPTY slice means "resolved to no matches" and returns no
	// rows — the same convention FilterIn already documents, for the
	// same reason: collapsing "no matches" into "no restriction" is the
	// more dangerous default.
	//
	// NOT used for TargetFilter's entity-join condition (uc-infra#78)
	// any more — see JoinFilters below for why that moved off this
	// mechanism. IDIn stays available as a generic "restrict to this
	// already-known id set" primitive for any future caller that
	// legitimately has one in hand.
	IDIn []string
	// JoinFilters are additional narrowing conditions requiring a
	// candidate record to have at least one matching row in ANOTHER
	// entity type, checked via a correlated EXISTS subquery — the
	// mechanism a FieldReference's declared TargetFilter entity-join
	// condition (Field.TargetFilter with Entity set) uses to narrow a
	// reference picker's candidates (uc-infra#78): e.g. "does a
	// PartyRole exist with party_id = this candidate's id AND
	// role_type = 'employee'?". Every entry ANDs together (every
	// condition on a field must hold), the same AND semantics
	// EqualsFilters already gives its own entries.
	//
	// Deliberately pushed down as a correlated EXISTS rather than
	// resolved application-side into a value list and re-bound as an
	// array parameter (RecordRepo.DistinctFieldValues, the ORIGINAL
	// shape this replaced) — independent review: that shape read the
	// ENTIRE matching set of the OTHER entity type into memory on every
	// debounced picker keystroke, unbounded, exactly the "will not work
	// at 1000s of rows" scale problem this endpoint exists to fix in
	// the first place. A correlated EXISTS lets Postgres decide per
	// candidate row whether a match exists, without ever materializing
	// the full joined set.
	JoinFilters []JoinFilter
	Limit       int
	Offset      int
}

// JoinFilter is one entity-join narrowing condition — see
// ListPageOptions.JoinFilters. Every field name and value is bound as a
// parameter, the same discipline every other generic field-name-driven
// query in this file follows.
type JoinFilter struct {
	// Entity is the OTHER entity type to check.
	Entity string
	// EntityField is the field on Entity that should hold the
	// candidate record's own id.
	EntityField string
	// Field/Value is the condition Entity's matching row must also
	// satisfy (e.g. role_type = "employee").
	Field string
	Value string
}

// FieldEquals is one exact-match field/value condition — see
// ListPageOptions.EqualsFilters.
type FieldEquals struct {
	Field string
	Value string
}

// CountFiltered is Count with the same optional filter ListPageFiltered
// applies, so the pager's total-pages math matches the rows actually
// returned. Without it, filtering to 3 rows would still show "Page 1 of
// 40".
func (r *RecordRepo) CountFiltered(ctx context.Context, entityType string, opts ListPageOptions) (int, error) {
	if opts.FilterField == "" && len(opts.EqualsFilters) == 0 && opts.IDIn == nil && len(opts.JoinFilters) == 0 {
		return r.CountByEntityType(ctx, entityType)
	}
	where, args := filterWhereClause(entityType, opts)
	var n int
	err := r.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM records WHERE %s`, where),
		args...,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count filtered records: %w", err)
	}
	return n, nil
}

// filterWhereClause builds the shared WHERE clause + bind args
// ListPageFiltered and CountFiltered both need: the entity/deleted_at
// scope every query here has, plus opts' optional label-search filter
// (FilterField/FilterValue or FilterIn), EqualsFilters, and IDIn — ANDed
// together so a reference-picker search's free-text filter and a
// FieldReference's declared TargetFilter/MustMatchParentField narrowing
// (uc-infra#78) compose instead of one replacing the other. Every
// caller-supplied field name and value is bound as a parameter, never
// concatenated into the query text, matching every other generic
// field-name-driven query in this file.
func filterWhereClause(entityType string, opts ListPageOptions) (string, []any) {
	args := []any{entityType}
	where := "entity_type = $1 AND deleted_at IS NULL"
	if opts.FilterField != "" {
		if opts.FilterIn != nil {
			args = append(args, opts.FilterField, opts.FilterIn)
			where += fmt.Sprintf(` AND data->>$%d = ANY($%d)`, len(args)-1, len(args))
		} else {
			args = append(args, opts.FilterField, "%"+escapeLike(opts.FilterValue)+"%")
			where += fmt.Sprintf(` AND data->>$%d ILIKE $%d ESCAPE '\'`, len(args)-1, len(args))
		}
	}
	for _, eq := range opts.EqualsFilters {
		args = append(args, eq.Field, eq.Value)
		where += fmt.Sprintf(` AND data->>$%d = $%d`, len(args)-1, len(args))
	}
	if opts.IDIn != nil {
		// id = ANY($n::uuid[]), not id::text = ANY($n): casting the
		// PARAMETER to uuid[] (Postgres infers the array element type
		// from the explicit cast) instead of casting the PK column to
		// text lets this use records' own primary-key btree index —
		// independent review: the previous id::text cast defeated it,
		// forcing a sequential scan of records for every call.
		args = append(args, opts.IDIn)
		where += fmt.Sprintf(` AND id = ANY($%d::uuid[])`, len(args))
	}
	for i, jf := range opts.JoinFilters {
		// A correlated EXISTS, not a resolve-then-array-bind — see
		// JoinFilters' own doc comment on why. records is referenced by
		// its bare table name from inside the subquery (the outer query
		// never aliases it either), which Postgres resolves as the
		// outer row per SQL's normal correlated-subquery scoping rules.
		// The subquery's own alias (j0, j1, ...) is unique per condition
		// so multiple JoinFilters entries don't collide with each other.
		args = append(args, jf.Entity, jf.EntityField, jf.Field, jf.Value)
		n := len(args)
		alias := fmt.Sprintf("j%d", i)
		where += fmt.Sprintf(
			` AND EXISTS (SELECT 1 FROM records %s WHERE %s.entity_type = $%d AND %s.deleted_at IS NULL AND %s.data->>$%d = records.id::text AND %s.data->>$%d = $%d)`,
			alias, alias, n-3, alias, alias, n-2, alias, n-1, n,
		)
	}
	return where, args
}

// escapeLike neutralises LIKE/ILIKE wildcards in a user filter value so a
// literal "%" or "_" matches itself, not "anything". Pairs with the
// ESCAPE '\' clause on the queries. Backslash first, so it does not
// double-escape the escapes it adds.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	s = strings.ReplaceAll(s, "_", "\\_")
	return s
}

// ListPageFiltered is ListPage with an optional sort and substring filter.
//
// The sort DIRECTION is chosen from opts.SortDesc as a SQL literal
// (ASC/DESC), never interpolated from caller text — direction is the one
// part of an ORDER BY that cannot be a bind parameter, so it must come
// from a closed set. The sort KEY and filter key/value are all bound
// parameters. `id` stays the final tiebreaker so a page boundary is
// deterministic even when many rows share the sort value.
func (r *RecordRepo) ListPageFiltered(ctx context.Context, entityType string, opts ListPageOptions) ([]Record, error) {
	where, args := filterWhereClause(entityType, opts)

	// created_at is a real column; a declared field sorts by its JSON
	// value. NULLS LAST keeps records missing the sort field from
	// dominating the first page of a descending sort.
	orderBy := "created_at"
	if opts.SortField != "" {
		args = append(args, opts.SortField)
		// A numeric field cast to numeric, so "10" sorts after "9" rather
		// than before it (text order) — wrong for money/qty is not a
		// cosmetic nit on an accounting product. The cast is guarded so a
		// non-numeric value in the field can't error the whole query
		// (data is JSON: a field could hold anything a bad import wrote).
		if opts.SortNumeric {
			orderBy = fmt.Sprintf(`(CASE WHEN data->>$%d ~ '^-?[0-9]+(\.[0-9]+)?$' THEN (data->>$%d)::numeric END)`, len(args), len(args))
		} else {
			orderBy = fmt.Sprintf("data->>$%d", len(args))
		}
	}
	dir := "ASC"
	if opts.SortDesc {
		dir = "DESC"
	}

	args = append(args, opts.Limit, opts.Offset)
	query := fmt.Sprintf(
		`SELECT id, data, version FROM records
		 WHERE %s
		 ORDER BY %s %s NULLS LAST, id
		 LIMIT $%d OFFSET $%d`,
		where, orderBy, dir, len(args)-1, len(args))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list filtered records page: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var id string
		var raw []byte
		var version int
		if err := rows.Scan(&id, &raw, &version); err != nil {
			return nil, fmt.Errorf("scan record: %w", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("unmarshal record data: %w", err)
		}
		out = append(out, Record{ID: id, EntityType: entityType, Data: data, Version: version})
	}
	return out, rows.Err()
}

// ListByField returns every non-deleted record of entityType whose data
// has fieldName == value — the query a master-detail section needs to
// find its child rows (e.g. every POLine whose purchase_order_id matches
// the parent PurchaseOrder's id). fieldName is passed as a bind
// parameter to the ->> operator, never concatenated into the query text,
// so a caller-controlled field name can't alter the query's structure.
func (r *RecordRepo) ListByField(ctx context.Context, entityType, fieldName, value string) ([]Record, error) {
	return r.listByField(ctx, r.db, entityType, fieldName, value, false)
}

// ListByFieldTx is ListByField against a caller-supplied querier/tx
// instead of r.db — same "same shape as the Tx write methods, now needed
// for a read too" reasoning GetTx/ListTx's own doc comments give. First
// real caller: assets.GenerateDepreciationScheduleOnWrite, which needs to
// read a FixedAsset's existing DepreciationSchedule rows from within the
// same transaction as the FixedAsset write that triggered it, to decide
// whether the schedule needs regenerating.
func (r *RecordRepo) ListByFieldTx(ctx context.Context, q querier, entityType, fieldName, value string) ([]Record, error) {
	return r.listByField(ctx, q, entityType, fieldName, value, false)
}

// ListByFieldForUpdateTx is ListByFieldTx with `SELECT ... FOR UPDATE` —
// for a caller that is about to decide whether to delete/replace the
// rows it reads based on their current state (e.g. "has anything here
// been posted yet?") and cannot let that decision race a concurrent
// writer. Without the row lock, a plain read here can observe a row as
// not-yet-posted, decide it's safe to delete, and then block on (and
// ultimately still delete) a row a concurrent transaction is in the
// middle of marking posted — the two-step "check, then act" any
// non-locking read+write pair is prone to under READ COMMITTED. First
// real caller: assets.GenerateDepreciationScheduleOnWrite's posted-row
// guard, which must not let a FixedAsset edit race
// internal/worker.Runner's depreciation-posting tick — see that hook's
// own doc comment for the concrete interleaving this closes.
func (r *RecordRepo) ListByFieldForUpdateTx(ctx context.Context, tx *sql.Tx, entityType, fieldName, value string) ([]Record, error) {
	return r.listByField(ctx, tx, entityType, fieldName, value, true)
}

func (r *RecordRepo) listByField(ctx context.Context, q querier, entityType, fieldName, value string, forUpdate bool) ([]Record, error) {
	query := `SELECT id, data, version FROM records
		 WHERE entity_type = $1 AND data->>$2 = $3 AND deleted_at IS NULL
		 ORDER BY created_at, id`
	if forUpdate {
		// FOR UPDATE requires a real transaction, not a bare *sql.DB
		// connection (which could span more than one physical connection
		// across calls) — enforced by taking *sql.Tx explicitly in
		// ListByFieldForUpdateTx's own signature rather than the generic
		// querier interface every other Tx method here accepts.
		query += " FOR UPDATE"
	}
	rows, err := q.QueryContext(ctx, query, entityType, fieldName, value)
	if err != nil {
		return nil, fmt.Errorf("list records by field: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var id string
		var raw []byte
		var version int
		if err := rows.Scan(&id, &raw, &version); err != nil {
			return nil, fmt.Errorf("scan record: %w", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("unmarshal record data: %w", err)
		}
		out = append(out, Record{ID: id, EntityType: entityType, Data: data, Version: version})
	}
	return out, rows.Err()
}

// ExistsByFieldsQ reports whether any non-deleted record of entityType
// has data[field] == value for EVERY field/value pair in equals — the
// query a FieldReference's declared TargetFilter join condition needs
// (uc-infra#78): does a PartyRole exist with party_id == this candidate
// Party's id AND role_type == "employee"? Used by crud.Engine
// (validating a write) and the reference-picker search endpoint
// (narrowing candidates) alike, so the two never risk diverging.
//
// Every field name and value is bound as a parameter to the ->>
// operator, never concatenated into the query text, the same discipline
// every other generic field-name-driven query in this file follows — a
// caller-controlled field name can only ever fail to match, never alter
// the query's structure.
//
// Takes the generic querier interface (not just *sql.DB) so crud.Engine
// can call it against its own write transaction — same reasoning as
// GetTx's own doc comment.
func (r *RecordRepo) ExistsByFieldsQ(ctx context.Context, q querier, entityType string, equals map[string]string) (bool, error) {
	if len(equals) == 0 {
		return false, fmt.Errorf("exists by fields on %s: at least one field/value pair is required", entityType)
	}
	args := []any{entityType}
	where := "entity_type = $1 AND deleted_at IS NULL"
	for field, value := range equals {
		args = append(args, field, value)
		where += fmt.Sprintf(" AND data->>$%d = $%d", len(args)-1, len(args))
	}
	var exists bool
	err := q.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM records WHERE %s)`, where),
		args...,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("exists by fields on %s: %w", entityType, err)
	}
	return exists, nil
}

// GetByFieldsQ is ExistsByFieldsQ's counterpart when the caller needs the
// matching record itself (id, data, version), not just whether one
// exists — first real caller: purchasing.creditInventoryOnReceipt
// (uc-infra#126), which must find the live InventoryItem for a given
// (item_id, facility_id) pair, inside the same transaction as the
// GoodsReceiptLine write that triggered it, to upsert rather than always
// inserting. found is false, err is nil, when no live record matches —
// ErrNotFound is deliberately NOT used here the way Get/GetTx use it:
// "no matching record" is the ordinary, expected outcome of an upsert's
// existence check, not an error condition the caller must unwrap.
//
// Same SQL-injection discipline as ExistsByFieldsQ: every field name and
// value is bound as a parameter to the ->> operator, never concatenated
// into the query text. When multiple live records match (equals doesn't
// name a Unique-declared field set, or the constraint predates this
// call), the one with the lowest (created_at, id) is returned — same
// "oldest wins" convention crud.BackfillUniqueConstraintKeys already
// established for the same kind of pre-existing-duplicate ambiguity, so
// the two mechanisms never disagree about which record is the canonical
// one. The `id` tiebreak (not just created_at) is required, not
// cosmetic (independent review, uc-infra#126): created_at defaults to
// now(), which is the enclosing transaction's start time in Postgres, so
// two records inserted in the SAME transaction (a bulk importer,
// recordmigrate) tie exactly — ORDER BY created_at alone would pick
// between them arbitrarily. ListPage and ListByField already order by
// (created_at, id) for this identical reason; this matches them.
func (r *RecordRepo) GetByFieldsQ(ctx context.Context, q querier, entityType string, equals map[string]string) (Record, bool, error) {
	if len(equals) == 0 {
		return Record{}, false, fmt.Errorf("get by fields on %s: at least one field/value pair is required", entityType)
	}
	args := []any{entityType}
	where := "entity_type = $1 AND deleted_at IS NULL"
	for field, value := range equals {
		args = append(args, field, value)
		where += fmt.Sprintf(" AND data->>$%d = $%d", len(args)-1, len(args))
	}
	var id string
	var raw []byte
	var version int
	err := q.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id, data, version FROM records WHERE %s ORDER BY created_at, id LIMIT 1`, where),
		args...,
	).Scan(&id, &raw, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("get by fields on %s: %w", entityType, err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return Record{}, false, fmt.Errorf("unmarshal record data: %w", err)
	}
	return Record{ID: id, EntityType: entityType, Data: data, Version: version}, true, nil
}

// DistinctFieldValues returns the distinct, non-empty string values
// resultField holds across every non-deleted record of entityType whose
// data has matchField == matchValue — resolves a FieldReference's
// declared TargetFilter join condition (e.g. every PartyRole.party_id
// whose role_type == "employee") to the candidate-id set the
// reference-picker search endpoint intersects its own label search
// against (see ListPageOptions.IDIn). A value that isn't a JSON string,
// or is empty, is skipped: resultField is always an id/reference column
// in every real use, so anything else there indicates corrupted data,
// not a match — the same "don't error the whole query over one bad row"
// posture ListPageFiltered's guarded numeric sort already takes.
func (r *RecordRepo) DistinctFieldValues(ctx context.Context, entityType, matchField, matchValue, resultField string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT DISTINCT data->>$1 FROM records
		 WHERE entity_type = $2 AND deleted_at IS NULL AND data->>$3 = $4`,
		resultField, entityType, matchField, matchValue,
	)
	if err != nil {
		return nil, fmt.Errorf("distinct field values on %s: %w", entityType, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan distinct field value: %w", err)
		}
		if v.Valid && v.String != "" {
			out = append(out, v.String)
		}
	}
	return out, rows.Err()
}

// Update replaces a record's data using the repo's own connection pool.
// Use UpdateTx when the write must be atomic with another operation.
// expectedVersion is optimistic-locking's whole point: nil means "don't
// check" (today's original unconditional-update behaviour, preserved so
// every caller that predates versioning keeps working unchanged); non-nil
// must match the record's current version or the update is rejected with
// ErrVersionConflict instead of silently clobbering a concurrent edit.
// Returns the record's new version on success.
func (r *RecordRepo) Update(ctx context.Context, entityType, id string, data map[string]any, expectedVersion *int) (int, error) {
	return r.UpdateTx(ctx, r.db, entityType, id, data, expectedVersion)
}

func (r *RecordRepo) UpdateTx(ctx context.Context, q querier, entityType, id string, data map[string]any, expectedVersion *int) (int, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("marshal record data: %w", err)
	}
	var newVersion int
	err = q.QueryRowContext(ctx,
		`UPDATE records SET data = $1, version = version + 1, updated_at = now()
		 WHERE id = $2 AND entity_type = $3 AND deleted_at IS NULL
		   AND ($4::int IS NULL OR version = $4)
		 RETURNING version`,
		raw, id, entityType, expectedVersion,
	).Scan(&newVersion)
	if err == nil {
		return newVersion, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("update record: %w", err)
	}
	// Zero rows updated — either the record doesn't exist (deleted, wrong
	// id) or it exists but expectedVersion didn't match. The single
	// UPDATE above can't distinguish those (its WHERE clause ANDs both
	// conditions together), so a follow-up existence check resolves
	// which error the caller actually needs: 404 vs. 409 are different
	// user-facing outcomes ("this record is gone" vs. "someone else just
	// changed it, reload and retry").
	var exists bool
	if checkErr := q.QueryRowContext(ctx,
		`SELECT true FROM records WHERE id = $1 AND entity_type = $2 AND deleted_at IS NULL`,
		id, entityType,
	).Scan(&exists); checkErr != nil {
		if errors.Is(checkErr, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("check record existence after failed update: %w", checkErr)
	}
	return 0, ErrVersionConflict
}

// Delete soft-deletes a record using the repo's own connection pool (no
// caller-supplied transaction). Use DeleteTx when the write must be
// atomic with another operation, such as an audit entry.
func (r *RecordRepo) Delete(ctx context.Context, entityType, id string) error {
	return r.DeleteTx(ctx, r.db, entityType, id)
}

func (r *RecordRepo) DeleteTx(ctx context.Context, q querier, entityType, id string) error {
	res, err := q.ExecContext(ctx,
		`UPDATE records SET deleted_at = now()
		 WHERE id = $1 AND entity_type = $2 AND deleted_at IS NULL`,
		id, entityType,
	)
	if err != nil {
		return fmt.Errorf("delete record: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountMissingField counts live records of entityType whose JSONB data
// has no usable value for fieldName — absent, JSON null, empty string,
// or a whitespace-only string.
//
// Used by cmd/sync-tenant-modules to report the data migrations a
// publish just made necessary (ADR-0017 §5). Publishing a Definition
// version that adds a Required field does not touch existing records,
// so they stay readable and start failing on next edit — a silent state
// this makes loud.
//
// Absent, JSON-null, an empty string, and a whitespace-only string all
// fail entity.ValidateRecord's Required check (#86, widened by #105) the
// same way, so this counts all four: a row holding `"facility_id": ""`
// or `"facility_id": "   "` is stock at no location regardless of which
// of the four shapes it's stored as, and it is exactly what a
// half-finished migration leaves behind.
//
// The explicit second argument to btrim (space/tab/newline/CR/vertical
// tab/form feed) is deliberate, not btrim's single-space default:
// without it, `btrim('  ')` only strips plain spaces and this query
// would silently miss a tab- or newline-only value even though
// strings.TrimSpace (and therefore entity.isBlankString) treats it as
// blank. This still isn't byte-for-byte identical to Go's Unicode-aware
// unicode.IsSpace (a NBSP/U+00A0-only value, for instance, isn't ASCII
// whitespace and won't match here) — an acknowledged, deliberately
// out-of-scope gap for a vanishingly rare shape, not something this
// migration-reporting query needs SQL-side Unicode class support to
// close.
func (r *RecordRepo) CountMissingField(ctx context.Context, entityType, fieldName string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM records
		 WHERE entity_type = $1
		   AND deleted_at IS NULL
		   AND btrim(coalesce(data->>$2, ''), E' \t\n\r\v\f') = ''`,
		entityType, fieldName,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count %s records missing %s: %w", entityType, fieldName, err)
	}
	return n, nil
}

// CountOutOfRangeField is CountMissingField's counterpart for a
// FieldNumber's declared entity.Field.Min/Max (uc-infra#80, ADR-0018 §4):
// a version bump that adds or tightens a numeric bound can invalidate
// existing records the exact same way a newly-Required field does — see
// cmd/sync-tenant-modules' numericBoundWarnings, which is what calls
// this. min/max are *float64 and passed through as-is: a nil bound
// becomes a SQL NULL, and the driver-level "IS NOT NULL" check below is
// what turns "no lower bound" into "never reject on the low side" rather
// than every row failing a `< NULL` comparison (which is neither true
// nor false in SQL, and would silently match nothing instead of erroring
// — the same care CountMissingField's own coalesce takes for a missing
// field).
//
// jsonb_typeof guards the cast: a record predating this field's
// existence, or one written with a non-numeric value before
// entity.ValidateRecord could catch it, must not fail the whole query
// with a Postgres cast error — such a row simply isn't counted as
// "out of range", the same "not this warning's problem" scope
// CountMissingField's Required check keeps for values it doesn't
// recognize as present.
func (r *RecordRepo) CountOutOfRangeField(ctx context.Context, entityType, fieldName string, min, max *float64) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM records
		 WHERE entity_type = $1
		   AND deleted_at IS NULL
		   AND jsonb_typeof(data->$2) = 'number'
		   AND (
		     ($3::float8 IS NOT NULL AND (data->>$2)::float8 < $3::float8)
		     OR ($4::float8 IS NOT NULL AND (data->>$2)::float8 > $4::float8)
		   )`,
		entityType, fieldName, min, max,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count %s records out of range on %s: %w", entityType, fieldName, err)
	}
	return n, nil
}
