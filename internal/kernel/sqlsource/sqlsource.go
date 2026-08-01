// Package sqlsource is the external-SQL-source import engine (ADR-0019):
// the vendor-neutral orchestration between a tenant's registered
// ExternalSQLSource (a foundation entity, credentials encrypted via
// secretcrypt), internal/data's read-only foreign-SQL access, and
// csvimport's mapping/preview/commit machinery. Like csvimport it knows
// formats and shapes, never vendors: everything NAV-specific in this
// package is data (templates.go / nav2009.go), consumed by the same
// generic code paths a hand-built mapping uses.
//
// The one capability this layer adds over csvimport is mapping
// *constants*: a fixed value applied to a field on every imported row
// ("every NAV Customer is party_type=organization"). Legacy schemas
// routinely cannot satisfy a required enum field from any real column —
// that finding is recorded in ADR-0019 and is exactly the evidence the
// R4 sync-engine epic needs — and constants close the gap without
// teaching csvimport value transformation: they are injected as
// synthetic columns into the header+rows shape before
// PreviewRows/CommitRows ever see them, so the generic engine stays
// generic.
package sqlsource

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/universaltill/universal-core/internal/kernel/csvimport"
)

// MaxImportRows bounds one import run (ADR-0019: a one-shot pull is
// always explicitly bounded). A legacy table larger than this imports
// in several runs; the UI states the cap rather than silently
// truncating.
const MaxImportRows = 10000

// PreviewRowCount is how many rows the preview step fetches — enough to
// judge a mapping against real data without dragging a legacy table
// across the wire to render one page.
const PreviewRowCount = 20

// Source is a decrypted, ready-to-dial external source — the runtime
// form of one ExternalSQLSource record. Password is plaintext here and
// only here (this value never persists and never renders).
type Source struct {
	Name     string
	Driver   string // data.ExtDriverMSSQL | data.ExtDriverPostgres
	Host     string
	Port     int
	Database string
	Username string
	Password string
	Options  string // extra DSN query parameters, "k=v&k2=v2"
}

// DSN composes the driver-specific connection string. DSN syntax is a
// driver concern and lives here — stored records keep the individual
// fields (see the ExternalSQLSource entity's doc comment).
func (s Source) DSN() (string, error) {
	port := s.Port
	if port == 0 {
		switch s.Driver {
		case "mssql":
			port = 1433
		default:
			port = 5432
		}
	}
	q, err := url.ParseQuery(s.Options)
	if err != nil {
		return "", fmt.Errorf("source %q: options must be URL query syntax (k=v&k2=v2): %w", s.Name, err)
	}
	for key := range q {
		if deniedDSNOptions[strings.ToLower(key)] {
			return "", fmt.Errorf("source %q: option %q is not allowed", s.Name, key)
		}
	}
	u := url.URL{
		Host: fmt.Sprintf("%s:%d", s.Host, port),
	}
	if s.Username != "" {
		if s.Password != "" {
			u.User = url.UserPassword(s.Username, s.Password)
		} else {
			u.User = url.User(s.Username)
		}
	}
	switch s.Driver {
	case "mssql":
		u.Scheme = "sqlserver"
		q.Set("database", s.Database)
	case "postgres":
		u.Scheme = "postgres"
		u.Path = "/" + s.Database
	default:
		return "", fmt.Errorf("source %q: unsupported driver %q", s.Name, s.Driver)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// deniedDSNOptions are option keys a tenant may not smuggle into a DSN.
// The dangerous class is options that make the DRIVER read files from
// OUR server's filesystem or override the credentials/identity the
// stored record declares: pgx's passfile would send a credential read
// from the server's own ~/.pgpass to whatever host the source points
// at, and sslcert/sslkey would present a server-local file as a TLS
// client certificate — both are file-exfiltration primitives when the
// "legacy ERP" host is attacker-chosen (ADR-0019's threat note).
// Deny-listed rather than allow-listed because legitimate driver
// options are many, driver-specific, and harmless (timeouts, encoding,
// application_name); the file/identity class is small and known.
var deniedDSNOptions = map[string]bool{
	// pgx / libpq file-reading and identity-overriding keys.
	"passfile": true, "sslcert": true, "sslkey": true,
	"sslrootcert": true, "sslcrl": true, "sslpassword": true,
	"service": true, "servicefile": true,
	"user": true, "password": true, "host": true, "port": true,
	"dbname": true, "database": true,
	// go-mssqldb credential/identity keys (database is shared above).
	"user id": true, "certificate": true,
}

// constPrefix marks the synthetic columns ApplyConstants injects. The
// prefix keeps them from ever colliding with a real source column (no
// SQL identifier a discovery listed will start with it, because
// ApplyConstants rejects sources that somehow do).
const constPrefix = "__const:"

// ApplyConstants returns headers+rows with one synthetic column per
// constant appended, plus the mapping entries that route each synthetic
// column to its target field. The returned slices are copies; the
// inputs are never mutated (callers reuse the fetched rows for both
// preview and commit).
func ApplyConstants(headers []string, rows [][]string, mapping csvimport.ColumnMapping, constants map[string]string) ([]string, [][]string, csvimport.ColumnMapping, error) {
	for _, h := range headers {
		if strings.HasPrefix(h, constPrefix) {
			return nil, nil, nil, fmt.Errorf("source column %q collides with the reserved constant-column prefix %q", h, constPrefix)
		}
	}
	outHeaders := append([]string(nil), headers...)
	outMapping := make(csvimport.ColumnMapping, len(mapping)+len(constants))
	maps.Copy(outMapping, mapping)
	// Deterministic column order: sorted by field name, so previews and
	// tests see a stable shape.
	fields := slices.Sorted(maps.Keys(constants))
	for _, f := range fields {
		col := constPrefix + f
		outHeaders = append(outHeaders, col)
		outMapping[col] = f
	}
	outRows := make([][]string, len(rows))
	for i, row := range rows {
		r := append([]string(nil), row...)
		for _, f := range fields {
			r = append(r, constants[f])
		}
		outRows[i] = r
	}
	return outHeaders, outRows, outMapping, nil
}
