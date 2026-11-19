package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationFiles holds the numbered schema migrations. A merged file is never edited: its
// checksum is recorded when it is applied, and a changed file stops the server from starting.
// The whole directory is embedded, not only *.sql, so a misnamed file such as 0002_x.SQL reaches
// the name check instead of being silently left out (embed still skips names starting with . or _).
//
//go:embed migrations
var migrationFiles embed.FS

// migrationLockClass is the first key of the two-key advisory lock that serializes migration
// runners; the second is the hash of the current schema. Two-key locks never collide with the
// single-key pg_advisory_xact_lock(hashtextextended(...)) locks the store takes elsewhere.
const migrationLockClass = 0x76326d67 // "v2mg"

// migrationsTable records every applied migration in the connection's current schema.
const migrationsTable = "v2_schema_migrations"

var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// transactionControl matches a line that starts a transaction-control statement. Every migration
// runs inside the runner's transaction together with its history row; a COMMIT in the file would
// commit its DDL without the row and leave every later start failing on the half-applied file.
// Only the statement's own grammar may follow the keyword (WORK/TRANSACTION, AND [NO] CHAIN, or
// BEGIN/START TRANSACTION modes), so "END AS x;" closing a CASE expression is not mistaken for END.
var transactionControl = regexp.MustCompile(`(?i)^\s*(?:(?:commit|rollback|abort|end)(?:\s+(?:work|transaction))?` +
	`(?:\s+and\s+(?:no\s+)?chain)?|(?:begin|start\s+transaction)(?:\s+[a-z ,]*)?)\s*;\s*(?:--.*)?$`)

// dollarQuote matches a PostgreSQL dollar-quote delimiter ($$ or $tag$).
var dollarQuote = regexp.MustCompile(`\$[A-Za-z_][A-Za-z_0-9]*\$|\$\$`)

// checkTransactionControl rejects top-level BEGIN/COMMIT/ROLLBACK/ABORT/END/START TRANSACTION
// statements. It is a line-based heuristic, not a SQL parser: it only sees a statement that starts
// a line and ends with ";" on that line, skips lines inside dollar-quoted bodies (so PL/pgSQL
// BEGIN ... END; blocks pass) by counting delimiters per line, and does not understand comments,
// string literals or several statements on one line. It catches the usual mistake, not every one.
func checkTransactionControl(name, sql string) error {
	inBody := false
	for i, line := range strings.Split(sql, "\n") {
		if !inBody && transactionControl.MatchString(line) {
			return fmt.Errorf("migration %s line %d: %q controls the transaction; each migration already runs "+
				"in one transaction, remove it", name, i+1, strings.TrimSpace(line))
		}
		if len(dollarQuote.FindAllString(line, -1))%2 == 1 {
			inBody = !inBody
		}
	}
	return nil
}

// AppliedMigration names a migration this process applied while opening the store.
type AppliedMigration struct {
	Version int
	Name    string
}

type migration struct {
	version  int
	name     string // file name, e.g. 0001_initial.sql
	sql      string
	checksum string // hex SHA-256 of the file bytes
}

type appliedRow struct {
	version  int
	name     string
	checksum string
}

// embeddedMigrations returns the migrations compiled into the binary.
func embeddedMigrations() (fs.FS, error) {
	sub, e := fs.Sub(migrationFiles, "migrations")
	if e != nil {
		return nil, fmt.Errorf("open embedded migrations: %w", e)
	}
	return sub, nil
}

// loadMigrations reads every file at the root of fsys. Names must match NNNN_snake_name.sql and
// versions must run 1..N without gaps or duplicates, so a misnumbered file fails the build's tests
// and every startup instead of being applied out of order.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, e := fs.ReadDir(fsys, ".")
	if e != nil {
		return nil, fmt.Errorf("list migrations: %w", e)
	}
	var out []migration
	for _, entry := range entries {
		name := entry.Name()
		match := migrationName.FindStringSubmatch(name)
		if entry.IsDir() || match == nil {
			return nil, fmt.Errorf("migration file %q: name must match NNNN_snake_case_name.sql", name)
		}
		data, e := fs.ReadFile(fsys, name)
		if e != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, e)
		}
		if e := checkTransactionControl(name, string(data)); e != nil {
			return nil, e
		}
		version, _ := strconv.Atoi(match[1])
		sum := sha256.Sum256(data)
		out = append(out, migration{version: version, name: name, sql: string(data), checksum: hex.EncodeToString(sum[:])})
	}
	if len(out) == 0 {
		return nil, errors.New("no migrations found")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	for i, m := range out {
		switch {
		case i > 0 && m.version == out[i-1].version:
			return nil, fmt.Errorf("migrations %s and %s share version %04d", out[i-1].name, m.name, m.version)
		case m.version != i+1:
			return nil, fmt.Errorf("migration %s: expected version %04d, versions must start at 0001 and be contiguous", m.name, i+1)
		}
	}
	return out, nil
}

// pendingMigrations checks the applied history against the known files and returns the files
// still to apply. Only versions above the highest applied one may run: a file numbered below
// the head would run last here but in the middle on a fresh database, so the two would diverge.
func pendingMigrations(known []migration, applied []appliedRow) ([]migration, error) {
	for i, row := range applied {
		if row.version < 1 {
			return nil, fmt.Errorf("invalid migration history: row with version %d (%s); versions start at 0001",
				row.version, row.name)
		}
		if row.version > len(known) {
			return nil, fmt.Errorf("database is newer than this binary: migration %04d (%s) is applied but unknown; "+
				"this binary knows migrations up to %04d", row.version, row.name, len(known))
		}
		m := known[row.version-1]
		if row.checksum != m.checksum {
			return nil, fmt.Errorf("migration %s changed after it was applied (checksum %s, applied %s); "+
				"never edit a merged migration, add a new one", m.name, m.checksum, row.checksum)
		}
		if row.name != m.name {
			return nil, fmt.Errorf("migration %04d was applied as %s but is now named %s; never rename a merged migration",
				row.version, row.name, m.name)
		}
		if row.version != i+1 {
			return nil, fmt.Errorf("migration %s is not applied but later migration %04d is; "+
				"a migration was inserted below the latest applied version, renumber it above %04d",
				known[i].name, row.version, applied[len(applied)-1].version)
		}
	}
	return known[len(applied):], nil
}

// migrate applies pending migrations from fsys to the connection's current schema. Each
// migration commits in its own transaction together with its history row, under a
// transaction-scoped advisory lock on the schema, so concurrent starters apply each migration
// exactly once and a failed migration leaves the database at the previous version. The error of
// a failed start names the migrations it committed before failing, so an operator knows how far
// the database moved before deciding whether to restore the backup.
func migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) ([]AppliedMigration, error) {
	known, e := loadMigrations(fsys)
	if e != nil {
		return nil, e
	}
	var done []AppliedMigration
	for {
		var next *migration
		// READ COMMITTED regardless of the database or role default: a starter that waited on the
		// advisory lock must see the history the previous holder committed, which a REPEATABLE READ
		// snapshot taken before the lock would hide.
		e := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
			var e error
			next, e = migrateStep(ctx, tx, known)
			return e
		})
		if e != nil {
			if len(done) > 0 {
				names := make([]string, len(done))
				for i, m := range done {
					names[i] = m.Name
				}
				e = fmt.Errorf("%w (this start already applied %s)", e, strings.Join(names, ", "))
			}
			return done, e
		}
		if next == nil {
			return done, nil
		}
		done = append(done, AppliedMigration{Version: next.version, Name: next.name})
	}
}

// migrateStep applies at most one migration inside tx and returns it, or nil when the schema is
// current. The history is re-read under the lock, so a concurrent starter's commits are seen.
func migrateStep(ctx context.Context, tx pgx.Tx, known []migration) (*migration, error) {
	var schema *string
	if e := tx.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); e != nil {
		return nil, fmt.Errorf("read current schema: %w", e)
	}
	if schema == nil {
		return nil, errors.New("no current schema: search_path names no existing schema")
	}
	if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, migrationLockClass, *schema); e != nil {
		return nil, fmt.Errorf("lock schema %s for migrations: %w", *schema, e)
	}
	table := pgx.Identifier{*schema, migrationsTable}.Sanitize()
	var tracked, legacy bool
	if e := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		               WHERE n.nspname = $1 AND c.relname = $2),
		       EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		               WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')
		                 AND (c.relname IN ('users', 'tenants', 'sessions', 'memberships') OR c.relname LIKE 'v2\_%'))`,
		*schema, migrationsTable).Scan(&tracked, &legacy); e != nil {
		return nil, fmt.Errorf("inspect schema %s: %w", *schema, e)
	}
	var applied []appliedRow
	if tracked {
		rows, e := tx.Query(ctx, `SELECT version, name, checksum FROM `+table+` ORDER BY version`)
		if e != nil {
			return nil, fmt.Errorf("read applied migrations: %w", e)
		}
		applied, e = pgx.CollectRows(rows, func(row pgx.CollectableRow) (appliedRow, error) {
			var r appliedRow
			return r, row.Scan(&r.version, &r.name, &r.checksum)
		})
		if e != nil {
			return nil, fmt.Errorf("read applied migrations: %w", e)
		}
	} else if legacy {
		return nil, fmt.Errorf("schema %s predates versioned migrations (it has v2-server tables but no %s); "+
			"back it up if needed, then drop and recreate the database", *schema, migrationsTable)
	}
	pending, e := pendingMigrations(known, applied)
	if e != nil {
		return nil, e
	}
	if len(pending) == 0 {
		return nil, nil
	}
	if !tracked {
		if _, e := tx.Exec(ctx, `CREATE TABLE `+table+` (
			version    integer PRIMARY KEY,
			name       text NOT NULL,
			checksum   text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); e != nil {
			return nil, fmt.Errorf("create %s: %w", migrationsTable, e)
		}
	}
	m := pending[0]
	if _, e := tx.Exec(ctx, m.sql); e != nil {
		return nil, fmt.Errorf("apply migration %s: %w", m.name, e)
	}
	if _, e := tx.Exec(ctx, `INSERT INTO `+table+` (version, name, checksum) VALUES ($1, $2, $3)`,
		m.version, m.name, m.checksum); e != nil {
		return nil, fmt.Errorf("record migration %s: %w", m.name, e)
	}
	return &m, nil
}
