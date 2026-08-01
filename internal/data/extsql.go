// External SQL sources (ADR-0019): the single place in this codebase
// that connects to and queries a database that is NOT ours — a tenant's
// legacy ERP (NAV 2009's SQL Server being the case the feature exists
// for), or any Postgres/MSSQL database they register. The repository
// rule ("raw SQL lives only in internal/data") applies to foreign SQL
// exactly as it does to our own: kernel packages
// (internal/kernel/sqlsource) get relations, columns and stringified
// rows from this file, never SQL text.
//
// Everything here is read-only BY CONSTRUCTION: the only statements this
// file can emit are the fixed INFORMATION_SCHEMA discovery queries and a
// generated `SELECT ... FROM <one quoted identifier>`. There is no code
// path that interpolates a caller-supplied SQL fragment, and no
// statement verb other than SELECT anywhere in the file — that property
// is the product promise ADR-0019 records ("we can look at your live
// ERP without being able to touch it") and the thing a reviewer of this
// file must re-verify before approving any change to it.
package data

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	// Register the drivers external sources may use. pgx is the same
	// driver the rest of the codebase already uses for our own database;
	// go-mssqldb exists solely for external sources (nothing of ours
	// runs on SQL Server).
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
)

// Drivers this package accepts for an external source. "pgx" is already
// this codebase's own driver; "sqlserver" is registered by the
// go-mssqldb import below. The public names match the
// ExternalSQLSource entity's driver enum, not Go driver-registry names.
const (
	ExtDriverMSSQL    = "mssql"
	ExtDriverPostgres = "postgres"
)

// sqlDriverName maps the public driver name to the database/sql
// registered driver that serves it.
var sqlDriverName = map[string]string{
	ExtDriverMSSQL:    "sqlserver",
	ExtDriverPostgres: "pgx",
}

// ExtSQL is an open, read-only-by-construction handle to an external
// SQL database.
type ExtSQL struct {
	db     *sql.DB
	driver string
}

// OpenExtSQL opens a handle to an external database. driver is one of
// ExtDriverMSSQL/ExtDriverPostgres; dsn is the driver-specific
// connection string (composed by internal/kernel/sqlsource, which owns
// DSN syntax). Open does not dial — call Ping to actually verify
// reachability, so a settings page can distinguish "saved" from
// "verified reachable".
func OpenExtSQL(driver, dsn string) (*ExtSQL, error) {
	name, ok := sqlDriverName[driver]
	if !ok {
		return nil, fmt.Errorf("unsupported external SQL driver %q (supported: %s, %s)", driver, ExtDriverMSSQL, ExtDriverPostgres)
	}
	db, err := sql.Open(name, dsn)
	if err != nil {
		return nil, fmt.Errorf("open external %s source: %w", driver, err)
	}
	// One connection is all an import run needs, and legacy ERP databases
	// are exactly the place not to open connection bursts against.
	db.SetMaxOpenConns(1)
	return &ExtSQL{db: db, driver: driver}, nil
}

func (e *ExtSQL) Close() error { return e.db.Close() }

// Ping verifies the source is actually reachable with the stored
// credentials.
func (e *ExtSQL) Ping(ctx context.Context) error { return e.db.PingContext(ctx) }

// ExtRelation is one importable relation (table or view) in the
// external database.
type ExtRelation struct {
	Schema string
	Name   string
	Kind   string // "table" | "view"
}

// Relations lists the external database's tables and views via
// INFORMATION_SCHEMA — the one metadata surface SQL Server and Postgres
// genuinely share — excluding each engine's own system schemas.
func (e *ExtSQL) Relations(ctx context.Context) ([]ExtRelation, error) {
	// Static text, no parameters: the exclusions are engine constants,
	// not caller input.
	q := `SELECT table_schema, table_name, table_type
FROM information_schema.tables
WHERE table_type IN ('BASE TABLE', 'VIEW')
  AND table_schema NOT IN ('pg_catalog', 'information_schema', 'sys')
ORDER BY table_schema, table_name`
	rows, err := e.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list external relations: %w", err)
	}
	defer rows.Close()
	var out []ExtRelation
	for rows.Next() {
		var r ExtRelation
		var typ string
		if err := rows.Scan(&r.Schema, &r.Name, &typ); err != nil {
			return nil, fmt.Errorf("scan external relation: %w", err)
		}
		if typ == "VIEW" {
			r.Kind = "view"
		} else {
			r.Kind = "table"
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FetchRows reads up to limit rows of schema.name and returns them as a
// header row plus stringified data rows — exactly the shape
// csvimport.PreviewRows/CommitRows consume, so an external relation and
// an uploaded file are indistinguishable to everything downstream.
// limit must be positive: an import run is always explicitly bounded
// (ADR-0019's one-shot pull), never "the whole table by default".
func (e *ExtSQL) FetchRows(ctx context.Context, schema, name string, limit int) (headers []string, rows [][]string, err error) {
	if limit <= 0 {
		return nil, nil, fmt.Errorf("external fetch requires a positive row limit, got %d", limit)
	}
	rel, err := e.quoteRelation(schema, name)
	if err != nil {
		return nil, nil, err
	}
	var q string
	switch e.driver {
	case ExtDriverMSSQL:
		q = fmt.Sprintf("SELECT TOP %d * FROM %s", limit, rel)
	default:
		q = fmt.Sprintf("SELECT * FROM %s LIMIT %d", rel, limit)
	}
	res, err := e.db.QueryContext(ctx, q)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch rows from %s.%s: %w", schema, name, err)
	}
	defer res.Close()

	headers, err = res.Columns()
	if err != nil {
		return nil, nil, fmt.Errorf("read columns of %s.%s: %w", schema, name, err)
	}
	for res.Next() {
		vals := make([]any, len(headers))
		ptrs := make([]any, len(headers))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := res.Scan(ptrs...); err != nil {
			return nil, nil, fmt.Errorf("scan row from %s.%s: %w", schema, name, err)
		}
		row := make([]string, len(headers))
		for i, v := range vals {
			row[i] = stringifyExtValue(v)
		}
		rows = append(rows, row)
	}
	return headers, rows, res.Err()
}

// quoteRelation quotes schema.name for this driver's identifier syntax.
// NAV 2009 per-company table names contain `$`, spaces, and dots
// ("CRONUS International Ltd_$Item"), so quoting is a correctness
// requirement, not hygiene. Quote characters inside the identifier are
// escaped by doubling, per both engines' rules; NUL is rejected because
// neither engine allows it and it can truncate identifiers in C-based
// layers.
func (e *ExtSQL) quoteRelation(schema, name string) (string, error) {
	q := func(ident string) (string, error) {
		if ident == "" {
			return "", fmt.Errorf("empty identifier")
		}
		if strings.ContainsRune(ident, 0) {
			return "", fmt.Errorf("identifier contains NUL")
		}
		switch e.driver {
		case ExtDriverMSSQL:
			return "[" + strings.ReplaceAll(ident, "]", "]]") + "]", nil
		default:
			return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`, nil
		}
	}
	qs, err := q(schema)
	if err != nil {
		return "", fmt.Errorf("bad schema name %q: %w", schema, err)
	}
	qn, err := q(name)
	if err != nil {
		return "", fmt.Errorf("bad relation name %q: %w", name, err)
	}
	return qs + "." + qn, nil
}

// stringifyExtValue renders one scanned external value the way a CSV
// cell would carry it, because csvimport.Coerce is what parses it next:
// NULL becomes the empty string (csvimport's blank-cell-is-absent
// convention), dates render as ISO-8601 (date-only when there is no
// time-of-day, matching entity.FieldDate's expected "2006-01-02"
// shape — NAV datetime columns carry midnight for pure dates), and
// numbers/bools render in the forms strconv accepts.
func stringifyExtValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []byte:
		// Both drivers deliver some types as raw bytes. Textual ones
		// (pgx numerics, go-mssqldb decimals/money — which arrive as
		// decimal *strings*) pass through; genuinely binary ones (NAV's
		// per-table rowversion binary(8), uniqueidentifier's 16 raw
		// bytes, blobs) would otherwise inject invalid UTF-8 into a
		// utf-8 response — and Postgres rejects it outright if a user
		// maps such a column ("invalid byte sequence for encoding
		// UTF8"). Hex is the honest, mappable-as-a-plain-string form.
		if utf8.Valid(t) {
			return string(t)
		}
		return "0x" + hex.EncodeToString(t)
	case string:
		return t
	case time.Time:
		if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
			return t.Format("2006-01-02")
		}
		return t.Format(time.RFC3339)
	case bool:
		return strconv.FormatBool(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}
