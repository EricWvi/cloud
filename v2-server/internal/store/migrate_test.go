package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationSchema creates an empty schema on TEST_DATABASE_URL, dropped after the test, and
// returns a pool config whose current schema it is.
func migrationSchema(t *testing.T) (*pgxpool.Config, *pgx.Conn, string) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { admin.Close(ctx) })
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "migrate_" + hex.EncodeToString(b[:])
	if _, e := admin.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") })
	config, e := pgxpool.ParseConfig(url)
	if e != nil {
		t.Fatal(e)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	return config, admin, schema
}

func mustOpen(t *testing.T, config *pgxpool.Config, migrations fs.FS) *Store {
	t.Helper()
	s, e := openWith(context.Background(), config, migrations)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	return s
}

func embedded(t *testing.T) fs.FS {
	t.Helper()
	m, e := embeddedMigrations()
	if e != nil {
		t.Fatal(e)
	}
	return m
}

// subset copies the first n files of migrations into a test file set, optionally extended.
func subset(t *testing.T, migrations fs.FS, n int, extra fstest.MapFS) fstest.MapFS {
	t.Helper()
	known, e := loadMigrations(migrations)
	if e != nil {
		t.Fatal(e)
	}
	out := fstest.MapFS{}
	for _, m := range known[:n] {
		out[m.name] = &fstest.MapFile{Data: []byte(m.sql)}
	}
	for name, f := range extra {
		out[name] = f
	}
	return out
}

func checksum(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func historyRows(t *testing.T, s *Store) []string {
	t.Helper()
	rows, e := s.pool.Query(context.Background(),
		`SELECT version || ' ' || name || ' ' || checksum || '|' || applied_at FROM v2_schema_migrations ORDER BY version`)
	if e != nil {
		t.Fatal(e)
	}
	out, e := pgx.CollectRows(rows, pgx.RowTo[string])
	if e != nil {
		t.Fatal(e)
	}
	return out
}

// catalog describes every object in the store's current schema with the schema name replaced,
// so two schemas built differently can be compared. It covers relations and their kinds,
// columns with types, defaults and nullability, constraints, indexes, functions, triggers, enum
// labels in order, view definitions and sequence parameters.
func catalog(t *testing.T, s *Store) []string {
	t.Helper()
	ctx := context.Background()
	var schema string
	if e := s.pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); e != nil {
		t.Fatal(e)
	}
	rows, e := s.pool.Query(ctx, `
		SELECT 'relation ' || c.relname || ' ' || c.relkind::text
		FROM pg_class c WHERE c.relnamespace = $1::text::regnamespace
		UNION ALL
		SELECT 'column ' || c.relname || '.' || a.attname || ' #' || a.attnum || ' ' ||
		       format_type(a.atttypid, a.atttypmod) || CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE '' END ||
		       COALESCE(' DEFAULT ' || pg_get_expr(d.adbin, d.adrelid), '') ||
		       CASE WHEN a.attidentity <> '' THEN ' IDENTITY ' || a.attidentity::text ELSE '' END ||
		       CASE WHEN a.attgenerated <> '' THEN ' GENERATED ' || a.attgenerated::text ELSE '' END
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE c.relnamespace = $1::text::regnamespace AND a.attnum > 0 AND NOT a.attisdropped
		UNION ALL
		SELECT 'constraint ' || c.relname || '.' || k.conname || ' ' || k.contype::text || ' ' || pg_get_constraintdef(k.oid)
		FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid
		WHERE k.connamespace = $1::text::regnamespace
		UNION ALL
		SELECT 'index ' || indexname || ' ' || indexdef FROM pg_indexes WHERE schemaname = $1::text
		UNION ALL
		SELECT 'function ' || pg_get_functiondef(p.oid) FROM pg_proc p WHERE p.pronamespace = $1::text::regnamespace
		UNION ALL
		SELECT 'trigger ' || pg_get_triggerdef(g.oid) FROM pg_trigger g JOIN pg_class c ON c.oid = g.tgrelid
		WHERE c.relnamespace = $1::text::regnamespace AND NOT g.tgisinternal
		UNION ALL
		-- Position, not enumsortorder: ADD VALUE ... BEFORE yields fractional sort orders that differ
		-- from a fresh CREATE TYPE with the same label order.
		SELECT 'enum ' || t.typname || ' #' || row_number() OVER (PARTITION BY t.oid ORDER BY e.enumsortorder) || ' ' || e.enumlabel
		FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid WHERE t.typnamespace = $1::text::regnamespace
		UNION ALL
		SELECT 'view ' || c.relname || ' ' || c.relkind::text || ' ' || pg_get_viewdef(c.oid)
		FROM pg_class c WHERE c.relnamespace = $1::text::regnamespace AND c.relkind IN ('v', 'm')
		UNION ALL
		SELECT 'sequence ' || sequencename || ' ' || data_type::text || ' start ' || start_value || ' min ' || min_value ||
		       ' max ' || max_value || ' increment ' || increment_by || ' cycle ' || cycle || ' cache ' || cache_size
		FROM pg_sequences WHERE schemaname = $1::text`, schema)
	if e != nil {
		t.Fatal(e)
	}
	out, e := pgx.CollectRows(rows, pgx.RowTo[string])
	if e != nil {
		t.Fatal(e)
	}
	for i := range out {
		out[i] = strings.ReplaceAll(out[i], schema+".", "SCHEMA.")
	}
	slices.Sort(out)
	return out
}

func diffCatalogs(want, got []string) string {
	var b strings.Builder
	for _, line := range want {
		if !slices.Contains(got, line) {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}
	for _, line := range got {
		if !slices.Contains(want, line) {
			fmt.Fprintf(&b, "+ %s\n", line)
		}
	}
	return b.String()
}

func TestMigrateFreshDatabaseAppliesEveryMigration(t *testing.T) {
	config, _, _ := migrationSchema(t)
	migrations := embedded(t)
	known, e := loadMigrations(migrations)
	if e != nil {
		t.Fatal(e)
	}
	s := mustOpen(t, config, migrations)

	var want, gotApplied []string
	for _, m := range known {
		want = append(want, fmt.Sprintf("%d %s %s", m.version, m.name, checksum(m.sql)))
	}
	for _, m := range s.AppliedMigrations() {
		gotApplied = append(gotApplied, fmt.Sprintf("%d %s", m.Version, m.Name))
	}
	var got []string
	for _, row := range historyRows(t, s) {
		got = append(got, row[:strings.LastIndex(row, "|")]) // drop applied_at
	}
	if !slices.Equal(got, want) {
		t.Fatalf("fresh database history:\n got %q\nwant %q", got, want)
	}
	if len(gotApplied) != len(known) || gotApplied[0] != "1 0001_initial.sql" {
		t.Fatalf("fresh open reported applied %q, want all %d migrations", gotApplied, len(known))
	}
	// The schema works: 0001 created the tables the store reads.
	if _, e := s.Tenants(context.Background(), "00000000-0000-0000-0000-000000000000"); e != nil {
		t.Fatalf("store query after fresh migrate: %v", e)
	}
}

func TestMigrateRestartAppliesNothing(t *testing.T) {
	config, _, _ := migrationSchema(t)
	migrations := embedded(t)
	first, e := openWith(context.Background(), config, migrations)
	if e != nil {
		t.Fatal(e)
	}
	history, shape := historyRows(t, first), catalog(t, first)
	first.Close()

	again := mustOpen(t, config, migrations)
	if applied := again.AppliedMigrations(); len(applied) != 0 {
		t.Fatalf("restart applied %v, want nothing", applied)
	}
	if got := historyRows(t, again); !slices.Equal(got, history) {
		t.Fatalf("restart changed history:\n got %q\nwant %q", got, history)
	}
	if d := diffCatalogs(shape, catalog(t, again)); d != "" {
		t.Fatalf("restart changed the schema:\n%s", d)
	}
}

// TestMigrateUpgradeMatchesFreshBuild builds a database with migrations 1..k for every k below
// the head, opens it with the full set and requires the same catalog as a fresh build. A
// test-only migration after the real ones makes the upgrade path non-trivial while N is 1; it
// also proves the comparison notices a difference.
func TestMigrateUpgradeMatchesFreshBuild(t *testing.T) {
	head := embedded(t)
	known, e := loadMigrations(head)
	if e != nil {
		t.Fatal(e)
	}
	next := fmt.Sprintf("%04d_test_upgrade.sql", len(known)+1)
	extra := fstest.MapFS{next: &fstest.MapFile{Data: []byte(`
		ALTER TABLE users ADD COLUMN upgrade_probe text NOT NULL DEFAULT 'x';
		CREATE INDEX users_upgrade_probe ON users (upgrade_probe);
		CREATE TYPE upgrade_probe_state AS ENUM ('first', 'second');
		CREATE SEQUENCE upgrade_probe_seq START 7 INCREMENT 3;
		CREATE VIEW upgrade_probe_users AS SELECT id, upgrade_probe FROM users WHERE upgrade_probe <> 'y';`)}}

	for _, set := range []struct {
		name  string
		files fs.FS
		n     int
	}{
		{"embedded", head, len(known)},
		{"embedded+test", subset(t, head, len(known), extra), len(known) + 1},
	} {
		t.Run(set.name, func(t *testing.T) {
			freshConfig, _, _ := migrationSchema(t)
			want := catalog(t, mustOpen(t, freshConfig, set.files))
			for k := 0; k < set.n; k++ {
				t.Run(fmt.Sprintf("from_%04d", k), func(t *testing.T) {
					config, _, _ := migrationSchema(t)
					if k > 0 {
						previous, e := openWith(context.Background(), config, subset(t, set.files, k, nil))
						if e != nil {
							t.Fatal(e)
						}
						if got := len(previous.AppliedMigrations()); got != k {
							t.Fatalf("building version %04d applied %d migrations", k, got)
						}
						previous.Close()
					}
					upgraded := mustOpen(t, config, set.files)
					if got := len(upgraded.AppliedMigrations()); got != set.n-k {
						t.Fatalf("upgrade from %04d applied %d migrations, want %d", k, got, set.n-k)
					}
					if d := diffCatalogs(want, catalog(t, upgraded)); d != "" {
						t.Fatalf("upgrade from %04d differs from a fresh build:\n%s", k, d)
					}
				})
			}
		})
	}

	// The comparison is sensitive: the head without the test migration differs from the head with it.
	a, _, _ := migrationSchema(t)
	b, _, _ := migrationSchema(t)
	without, with := catalog(t, mustOpen(t, a, head)), catalog(t, mustOpen(t, b, subset(t, head, len(known), extra)))
	d := diffCatalogs(without, with)
	for _, want := range []string{
		"+ column users.upgrade_probe", "+ index users_upgrade_probe", "+ enum upgrade_probe_state #2 second",
		"+ view upgrade_probe_users v  SELECT", "+ sequence upgrade_probe_seq bigint start 7",
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("catalog comparison missed %q:\n%s", want, d)
		}
	}
}

func TestLoadMigrationsRejectsInvalidSets(t *testing.T) {
	file := func(sql string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(sql)} }
	for _, tc := range []struct {
		name  string
		files fstest.MapFS
		want  string
	}{
		{"empty", fstest.MapFS{}, "no migrations found"},
		{"uppercase", fstest.MapFS{"0001_Initial.sql": file("")}, `"0001_Initial.sql": name must match`},
		{"short version", fstest.MapFS{"001_initial.sql": file("")}, `"001_initial.sql": name must match`},
		{"no name", fstest.MapFS{"0001.sql": file("")}, `"0001.sql": name must match`},
		{"not sql", fstest.MapFS{"0001_initial.sql": file(""), "README.md": file("")}, `"README.md": name must match`},
		{"directory", fstest.MapFS{"0001_initial.sql": file(""), "0002_dir.sql/x": file("")}, `"0002_dir.sql": name must match`},
		{"starts at two", fstest.MapFS{"0002_second.sql": file("")}, "migration 0002_second.sql: expected version 0001"},
		{"starts at zero", fstest.MapFS{"0000_zero.sql": file(""), "0001_one.sql": file("")}, "migration 0000_zero.sql: expected version 0001"},
		{"gap", fstest.MapFS{"0001_a.sql": file(""), "0003_c.sql": file("")}, "migration 0003_c.sql: expected version 0002"},
		{"duplicate", fstest.MapFS{"0001_a.sql": file(""), "0001_b.sql": file("")}, "migrations 0001_a.sql and 0001_b.sql share version 0001"},
		{"uppercase extension", fstest.MapFS{"0001_a.sql": file(""), "0002_b.SQL": file("")}, `"0002_b.SQL": name must match`},
		{"sql.txt", fstest.MapFS{"0001_a.sql": file(""), "0002_b.sql.txt": file("")}, `"0002_b.sql.txt": name must match`},
		{"begin", fstest.MapFS{"0001_a.sql": file("BEGIN;\nCREATE TABLE t (id int);\n")}, `migration 0001_a.sql line 1: "BEGIN;" controls the transaction`},
		{"commit", fstest.MapFS{"0001_a.sql": file("CREATE TABLE t (id int);\n  commit;\n")}, `migration 0001_a.sql line 2: "commit;" controls the transaction`},
		{"commit work", fstest.MapFS{"0001_a.sql": file("CREATE TABLE t (id int);\nCOMMIT WORK;")}, `line 2: "COMMIT WORK;"`},
		{"rollback", fstest.MapFS{"0001_a.sql": file("Rollback;")}, `line 1: "Rollback;"`},
		{"abort", fstest.MapFS{"0001_a.sql": file("ABORT;")}, `line 1: "ABORT;"`},
		{"end", fstest.MapFS{"0001_a.sql": file("CREATE TABLE t (id int);\nEND;")}, `line 2: "END;"`},
		{"start transaction", fstest.MapFS{"0001_a.sql": file("START TRANSACTION ISOLATION LEVEL SERIALIZABLE;")}, `line 1: "START TRANSACTION ISOLATION LEVEL SERIALIZABLE;"`},
		{"after a body", fstest.MapFS{"0001_a.sql": file("DO $$\nBEGIN\n  PERFORM 1;\nEND;\n$$;\nCOMMIT;")}, `line 6: "COMMIT;"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e := loadMigrations(tc.files)
			if e == nil || !strings.Contains(e.Error(), tc.want) {
				t.Fatalf("loadMigrations = %v, want error containing %q", e, tc.want)
			}
		})
	}
	known, e := loadMigrations(embedded(t))
	if e != nil {
		t.Fatalf("embedded migrations are invalid: %v", e)
	}
	// The binary embeds the whole directory, so a stray file on disk reaches the name check above
	// instead of being left out of the build.
	onDisk, e := os.ReadDir("migrations")
	if e != nil {
		t.Fatal(e)
	}
	var disk, built []string
	for _, entry := range onDisk {
		if !strings.HasPrefix(entry.Name(), ".") && !strings.HasPrefix(entry.Name(), "_") {
			disk = append(disk, entry.Name())
		}
	}
	builtEntries, e := fs.ReadDir(embedded(t), ".")
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range builtEntries {
		built = append(built, entry.Name())
	}
	if !slices.Equal(built, disk) {
		t.Fatalf("embedded migrations %q, directory has %q", built, disk)
	}
	if known[0].name != "0001_initial.sql" {
		t.Fatalf("first embedded migration = %s", known[0].name)
	}
}

// TestLoadMigrationsAllowsTransactionKeywordsInBodies keeps the transaction-control check from
// rejecting PL/pgSQL blocks and expressions that merely contain the keywords.
func TestLoadMigrationsAllowsTransactionKeywordsInBodies(t *testing.T) {
	for name, sql := range map[string]string{
		"do block": "DO $$\nBEGIN\n  IF true THEN\n    PERFORM 1;\n  END IF;\nEND;\n$$;\n",
		"function": "CREATE FUNCTION f() RETURNS int LANGUAGE plpgsql AS $body$\nBEGIN\n  RETURN 1;\nEND;\n$body$;\n",
		"case":     "CREATE VIEW v AS SELECT CASE WHEN true THEN 1\nEND AS x;\n",
		"columns":  "CREATE TABLE t (begin_at timestamptz, end_at timestamptz);\n",
		"comment":  "-- COMMIT; would be wrong here\nCREATE TABLE t (id int);\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := loadMigrations(fstest.MapFS{"0001_a.sql": {Data: []byte(sql)}}); e != nil {
				t.Fatalf("loadMigrations rejected %q: %v", sql, e)
			}
		})
	}
}

func TestPendingMigrations(t *testing.T) {
	known := []migration{
		{version: 1, name: "0001_a.sql", checksum: "a"},
		{version: 2, name: "0002_b.sql", checksum: "b"},
		{version: 3, name: "0003_c.sql", checksum: "c"},
	}
	row := func(v int, name, sum string) appliedRow { return appliedRow{version: v, name: name, checksum: sum} }
	for _, tc := range []struct {
		name    string
		applied []appliedRow
		pending []int
		want    string
	}{
		{"empty", nil, []int{1, 2, 3}, ""},
		{"partial", []appliedRow{row(1, "0001_a.sql", "a")}, []int{2, 3}, ""},
		{"current", []appliedRow{row(1, "0001_a.sql", "a"), row(2, "0002_b.sql", "b"), row(3, "0003_c.sql", "c")}, nil, ""},
		{"changed", []appliedRow{row(1, "0001_a.sql", "x")}, nil, "migration 0001_a.sql changed after it was applied"},
		{"renamed", []appliedRow{row(1, "0001_old.sql", "a")}, nil, "migration 0001 was applied as 0001_old.sql but is now named 0001_a.sql"},
		{"newer", []appliedRow{row(1, "0001_a.sql", "a"), row(2, "0002_b.sql", "b"), row(3, "0003_c.sql", "c"), row(4, "0004_d.sql", "d")}, nil,
			"database is newer than this binary: migration 0004 (0004_d.sql) is applied but unknown"},
		{"inserted below head", []appliedRow{row(1, "0001_a.sql", "a"), row(3, "0003_c.sql", "c")}, nil,
			"migration 0002_b.sql is not applied but later migration 0003 is"},
		{"missing first", []appliedRow{row(2, "0002_b.sql", "b")}, nil, "migration 0001_a.sql is not applied but later migration 0002 is"},
		{"version zero", []appliedRow{row(0, "0000_x.sql", "x"), row(1, "0001_a.sql", "a")}, nil,
			"invalid migration history: row with version 0 (0000_x.sql)"},
		{"negative version", []appliedRow{row(-1, "bad", "x")}, nil, "invalid migration history: row with version -1 (bad)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending, e := pendingMigrations(known, tc.applied)
			if tc.want != "" {
				if e == nil || !strings.Contains(e.Error(), tc.want) {
					t.Fatalf("pendingMigrations = %v, %v; want error containing %q", pending, e, tc.want)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			var got []int
			for _, m := range pending {
				got = append(got, m.version)
			}
			if !slices.Equal(got, tc.pending) {
				t.Fatalf("pending = %v, want %v", got, tc.pending)
			}
		})
	}
}

// TestOpenRefusesInvalidHistory proves every startup refusal reaches OpenConfig's caller and
// leaves the database untouched.
func TestOpenRefusesInvalidHistory(t *testing.T) {
	file := func(sql string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(sql)} }
	one := fstest.MapFS{"0001_one.sql": file("CREATE TABLE probe_one (id int);")}
	three := fstest.MapFS{
		"0001_one.sql":   file("CREATE TABLE probe_one (id int);"),
		"0002_two.sql":   file("CREATE TABLE probe_two (id int);"),
		"0003_three.sql": file("CREATE TABLE probe_three (id int);"),
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, config *pgxpool.Config, admin *pgx.Conn, schema string)
		open  fs.FS
		want  string
	}{
		{
			name:  "checksum mismatch",
			setup: func(t *testing.T, c *pgxpool.Config, _ *pgx.Conn, _ string) { mustOpen(t, c, one).Close() },
			open:  fstest.MapFS{"0001_one.sql": file("CREATE TABLE probe_one (id bigint);")},
			want:  "migration 0001_one.sql changed after it was applied",
		},
		{
			name:  "unknown newer version",
			setup: func(t *testing.T, c *pgxpool.Config, _ *pgx.Conn, _ string) { mustOpen(t, c, three).Close() },
			open:  one,
			want:  "database is newer than this binary: migration 0002 (0002_two.sql) is applied but unknown",
		},
		{
			name: "inserted below head",
			setup: func(t *testing.T, c *pgxpool.Config, admin *pgx.Conn, schema string) {
				mustOpen(t, c, three).Close()
				// The database has 1 and 3; the binary's 0002 was numbered below the applied head.
				if _, e := admin.Exec(context.Background(), `DROP TABLE `+schema+`.probe_two; DELETE FROM `+schema+`.v2_schema_migrations WHERE version = 2`); e != nil {
					t.Fatal(e)
				}
			},
			open: three,
			want: "migration 0002_two.sql is not applied but later migration 0003 is",
		},
		{
			name:  "bad file name",
			setup: func(*testing.T, *pgxpool.Config, *pgx.Conn, string) {},
			open:  fstest.MapFS{"0001_One.sql": file("CREATE TABLE probe_one (id int);")},
			want:  `migration file "0001_One.sql": name must match`,
		},
		{
			name: "database predates migrations",
			setup: func(t *testing.T, _ *pgxpool.Config, admin *pgx.Conn, schema string) {
				// The old schema.sql built tables without any history table.
				if _, e := admin.Exec(context.Background(), `CREATE TABLE `+schema+`.v2_spaces (id uuid)`); e != nil {
					t.Fatal(e)
				}
			},
			open: one,
			want: "predates versioned migrations",
		},
		{
			name: "old users table",
			setup: func(t *testing.T, _ *pgxpool.Config, admin *pgx.Conn, schema string) {
				if _, e := admin.Exec(context.Background(), `CREATE TABLE `+schema+`.users (id uuid)`); e != nil {
					t.Fatal(e)
				}
			},
			open: one,
			want: "predates versioned migrations",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, admin, schema := migrationSchema(t)
			tc.setup(t, config, admin, schema)
			before := schemaObjects(t, admin, schema)
			s, e := openWith(context.Background(), config, tc.open)
			if e == nil {
				s.Close()
				t.Fatalf("open succeeded, want error containing %q", tc.want)
			}
			if !strings.Contains(e.Error(), tc.want) {
				t.Fatalf("open error = %v, want it to contain %q", e, tc.want)
			}
			if after := schemaObjects(t, admin, schema); !slices.Equal(after, before) {
				t.Fatalf("refused open changed the schema:\nbefore %q\n after %q", before, after)
			}
		})
	}
}

// schemaObjects lists the schema's relations and history rows as the admin connection sees them.
func schemaObjects(t *testing.T, admin *pgx.Conn, schema string) []string {
	t.Helper()
	ctx := context.Background()
	rows, e := admin.Query(ctx, `SELECT relname FROM pg_class WHERE relnamespace = $1::text::regnamespace ORDER BY relname`, schema)
	if e != nil {
		t.Fatal(e)
	}
	out, e := pgx.CollectRows(rows, pgx.RowTo[string])
	if e != nil {
		t.Fatal(e)
	}
	if slices.Contains(out, migrationsTable) {
		rows, e := admin.Query(ctx, `SELECT version || ' ' || checksum FROM `+schema+`.v2_schema_migrations ORDER BY version`)
		if e != nil {
			t.Fatal(e)
		}
		history, e := pgx.CollectRows(rows, pgx.RowTo[string])
		if e != nil {
			t.Fatal(e)
		}
		out = append(out, history...)
	}
	return out
}

func TestMigrateConcurrentStartersApplyOnce(t *testing.T) {
	config, _, _ := migrationSchema(t)
	migrations := embedded(t)
	known, e := loadMigrations(migrations)
	if e != nil {
		t.Fatal(e)
	}
	const starters = 4
	var (
		start   = make(chan struct{})
		wg      sync.WaitGroup
		mu      sync.Mutex
		errs    []error
		applied []AppliedMigration
	)
	for range starters {
		wg.Go(func() {
			<-start
			s, e := openWith(context.Background(), config, migrations)
			mu.Lock()
			defer mu.Unlock()
			if e != nil {
				errs = append(errs, e)
				return
			}
			applied = append(applied, s.AppliedMigrations()...)
			s.Close()
		})
	}
	close(start)
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("concurrent starters failed: %v", errs)
	}
	if len(applied) != len(known) {
		t.Fatalf("concurrent starters applied %v in total, want each of %d migrations once", applied, len(known))
	}
	s := mustOpen(t, config, migrations)
	if rows := historyRows(t, s); len(rows) != len(known) {
		t.Fatalf("history after concurrent start = %q", rows)
	}
}

// TestMigrateFailureRollsBackAndRetries pins that a migration and its history row commit together:
// a 0002 that fails after running DDL, in its own SQL or while its history row is recorded, leaves
// neither its table nor its row, the error names what this start already applied, and a
// corrected 0002 then applies exactly once on the next start.
func TestMigrateFailureRollsBackAndRetries(t *testing.T) {
	file := func(sql string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(sql)} }
	one := "CREATE TABLE probe_one (id int);"
	for _, tc := range []struct {
		name, failing, want string
	}{
		{"statement fails", "CREATE TABLE probe_two (id int);\nSELECT 1/0;", "apply migration 0002_two.sql: ERROR: division by zero"},
		{
			// The DDL succeeds, then the history INSERT violates a constraint the file added: only a
			// shared transaction undoes the table along with the failed row.
			"history row fails",
			"CREATE TABLE probe_two (id int);\nALTER TABLE v2_schema_migrations ADD CONSTRAINT probe_refuse CHECK (version < 2);",
			"record migration 0002_two.sql",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, admin, schema := migrationSchema(t)
			_, e := openWith(context.Background(), config, fstest.MapFS{"0001_one.sql": file(one), "0002_two.sql": file(tc.failing)})
			if e == nil || !strings.Contains(e.Error(), tc.want) {
				t.Fatalf("open with failing 0002 = %v, want error containing %q", e, tc.want)
			}
			if !strings.Contains(e.Error(), "(this start already applied 0001_one.sql)") {
				t.Fatalf("error %q does not name the migration applied before the failure", e)
			}
			got := schemaObjects(t, admin, schema)
			want := []string{"probe_one", migrationsTable, migrationsTable + "_pkey", "1 " + checksum(one)}
			if !slices.Equal(got, want) {
				t.Fatalf("after the failed 0002 the schema has %q, want only 0001's table and history row %q", got, want)
			}
			var constraints int
			if e := admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_constraint WHERE conname = 'probe_refuse'`).Scan(&constraints); e != nil {
				t.Fatal(e)
			}
			if constraints != 0 {
				t.Fatal("the failed 0002's constraint survived")
			}

			s := mustOpen(t, config, fstest.MapFS{"0001_one.sql": file(one), "0002_two.sql": file("CREATE TABLE probe_two (id int);")})
			if applied := s.AppliedMigrations(); !slices.Equal(applied, []AppliedMigration{{Version: 2, Name: "0002_two.sql"}}) {
				t.Fatalf("corrected start applied %v, want exactly 0002", applied)
			}
			if rows := historyRows(t, s); len(rows) != 2 {
				t.Fatalf("history after the corrected start = %q", rows)
			}
		})
	}
}

// TestMigrateConcurrentStartersUnderRepeatableRead runs the concurrent start with a connection
// default of REPEATABLE READ: the runner must still see the history a lock holder committed.
func TestMigrateConcurrentStartersUnderRepeatableRead(t *testing.T) {
	config, _, _ := migrationSchema(t)
	config.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	migrations := embedded(t)
	known, e := loadMigrations(migrations)
	if e != nil {
		t.Fatal(e)
	}
	const starters = 4
	var (
		start   = make(chan struct{})
		wg      sync.WaitGroup
		mu      sync.Mutex
		errs    []error
		applied int
	)
	for range starters {
		wg.Go(func() {
			<-start
			s, e := openWith(context.Background(), config, migrations)
			mu.Lock()
			defer mu.Unlock()
			if e != nil {
				errs = append(errs, e)
				return
			}
			applied += len(s.AppliedMigrations())
			s.Close()
		})
	}
	close(start)
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("concurrent starters under repeatable read failed: %v", errs)
	}
	if applied != len(known) {
		t.Fatalf("concurrent starters applied %d migrations in total, want %d", applied, len(known))
	}
}

// TestOpenRefusesNonPositiveHistoryVersion returns an error, not a panic, for a hand-edited row.
func TestOpenRefusesNonPositiveHistoryVersion(t *testing.T) {
	config, admin, schema := migrationSchema(t)
	migrations := embedded(t)
	mustOpen(t, config, migrations).Close()
	if _, e := admin.Exec(context.Background(),
		`INSERT INTO `+schema+`.v2_schema_migrations (version, name, checksum) VALUES (0, 'bad.sql', 'x')`); e != nil {
		t.Fatal(e)
	}
	_, e := openWith(context.Background(), config, migrations)
	if e == nil || !strings.Contains(e.Error(), "invalid migration history: row with version 0 (bad.sql)") {
		t.Fatalf("open with a version-0 history row = %v", e)
	}
}
