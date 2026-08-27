package schemasql

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// schema_sql_parity_test.go — every column a Go SQL literal WRITES, against the
// schema `lens migrate` actually builds.
//
// ⚠ WHY THIS EXISTS. `go build` and `go vet` cannot see inside a string, so a column
// renamed or dropped by a later migration leaves every statement that names it
// compiling, passing review, shipping, and failing at runtime on whichever code path
// reaches it first. talyvor-docs established the technique (W3.18, #196); this
// repository has 123 migrations and raw SQL in ~40 packages and had no guard of that
// kind — the real-schema tests it does have each cover their own package's tables.
//
// ⚠ THE SCHEMA IS BUILT THE WAY A DEPLOYMENT BUILDS IT, not from a fixture and not
// by parsing the .sql files: dbmigrate.Run(ctx, conn, migrations.FS) is the exact
// call cmd/lens's `migrate` subcommand makes. W3.20 (talyvor-docs #197) is the
// reason that distinction is spelled out — a harness schema is not a deployment
// schema unless something proves it.
//
// ⚠ AND WHY IT ONLY CHECKS TWO SQL FORMS. A naive column extractor over 100+ raw
// literals produces false positives on aliases, CTE names, computed expressions,
// jsonb operators, EXCLUDED.x and table-qualified names — and a guard that cries
// wolf gets deleted. So it reads only the two shapes where a column name is
// unambiguous:
//
//	INSERT INTO <table> (a, b, c)      the column list
//	UPDATE <table> SET a = …, b = …    the assignment targets
//
// That is deliberately high-precision and low-recall. It covers the WRITE paths,
// where a wrong column name is a hard runtime error rather than a silent empty
// result — and it currently checks 939 references across the repository, which is
// the number the non-vacuity floor below is anchored on. SELECT column lists are NOT
// swept, and saying so plainly beats a guard whose coverage nobody can state.

const (
	// schemaCheckDB is created and dropped by this test. Fixed name rather than a
	// unique one: CI runs `go test -p 1`, so package binaries never overlap.
	schemaCheckDB = "lens_schemasql_parity"

	// checkedRefFloor — the sweep found 1036 column references (INSERT lists, UPDATE
	// SET targets and ON CONFLICT DO UPDATE SET targets) when this was written. A run that finds materially fewer has broken, and a broken
	// sweep reports a clean parity check.
	checkedRefFloor = 900

	// writeTargetFloor — 190 INSERT/UPDATE target tables were found when this was
	// written. Same reasoning as the reference floor: a sweep that finds a handful
	// has broken.
	writeTargetFloor = 150

	// schemaTableFloor — the migrated schema had 125 relations. Same reasoning: an
	// empty schema makes every reference "not found" or, worse, makes every table
	// unknown and therefore skipped.
	schemaTableFloor = 100
)

var (
	sqlLiteral = regexp.MustCompile("`([^`]*)`")
	insertForm = regexp.MustCompile(`(?is)INSERT\s+INTO\s+([a-z_][a-z0-9_]*)\s*\(([^)]*)\)`)
	// ⚠ `(?:^|[^O])` — NOT preceded by "DO". `INSERT … ON CONFLICT … DO UPDATE SET x = …`
	// is an upsert assignment against the INSERT's table, not an UPDATE of a table
	// called "set", and the first draft of this guard read all 20 of them that way:
	// it captured the table name as "set", found no such relation, and SKIPPED them.
	// They are picked up by upsertForm below and attributed to the enclosing INSERT.
	updateForm = regexp.MustCompile(`(?is)(?:^|[^O])UPDATE\s+([a-z_][a-z0-9_]*)\s+SET\s+(.*?)(?:\s+WHERE\b|\s+RETURNING\b|$)`)
	upsertForm = regexp.MustCompile(`(?is)ON\s+CONFLICT\b.*?DO\s+UPDATE\s+SET\s+(.*?)(?:\s+WHERE\b|\s+RETURNING\b|$)`)
	setTarget  = regexp.MustCompile(`(?i)^([a-z_][a-z0-9_]*)\s*=`)
	plainIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
)

// migratedSchema builds the deployment schema in its own database and returns
// table → column set.
func migratedSchema(t *testing.T) map[string]map[string]bool {
	t.Helper()
	adminURL := os.Getenv("LENS_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping schema/SQL parity guard")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect (admin): %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+schemaCheckDB); err != nil {
		t.Fatalf("drop %s: %v", schemaCheckDB, err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+schemaCheckDB); err != nil {
		t.Fatalf("create %s: %v", schemaCheckDB, err)
	}
	t.Cleanup(func() {
		c, cerr := pgx.Connect(context.Background(), adminURL)
		if cerr != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+schemaCheckDB)
	})

	target := swapDatabase(adminURL, schemaCheckDB)
	conn, err := pgx.Connect(ctx, target)
	if err != nil {
		t.Fatalf("connect (%s): %v", schemaCheckDB, err)
	}
	defer conn.Close(ctx)

	// The SAME call cmd/lens's migrate subcommand makes.
	applied, err := dbmigrate.Run(ctx, conn, migrations.FS)
	if err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("no migrations applied into a fresh database — the schema below would be empty " +
			"and every parity check would pass over nothing")
	}

	rows, err := conn.Query(ctx,
		`SELECT table_name, column_name FROM information_schema.columns WHERE table_schema = 'public'`)
	if err != nil {
		t.Fatalf("read information_schema: %v", err)
	}
	defer rows.Close()
	schema := map[string]map[string]bool{}
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if schema[tbl] == nil {
			schema[tbl] = map[string]bool{}
		}
		schema[tbl][col] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(schema) < schemaTableFloor {
		t.Fatalf("the migrated schema has %d relations, want >= %d — %d migrations applied but the "+
			"schema read is wrong, and an empty schema makes every table 'unknown' and therefore "+
			"skipped", len(schema), schemaTableFloor, len(applied))
	}
	return schema
}

// swapDatabase rewrites the database name in a postgres URL.
func swapDatabase(url, db string) string {
	q := ""
	if i := strings.Index(url, "?"); i >= 0 {
		q, url = url[i:], url[:i]
	}
	if i := strings.LastIndex(url, "/"); i >= 0 {
		url = url[:i+1] + db
	}
	return url + q
}

// writeTargetTables returns every table an INSERT/UPDATE in a Go SQL literal targets.
// Measured before this was asserted: with the DO-UPDATE artefact excluded, EVERY such
// table is in the migrated schema, so requiring it costs no false positives and closes
// the hole where a mistyped TABLE name is silently skipped rather than reported.
type colRef struct {
	file  string
	line  int
	form  string
	table string
	col   string
}

// writeColumnRefs sweeps every non-test .go file for the two unambiguous forms.
func writeColumnRefs(t *testing.T, schema map[string]map[string]bool) []colRef {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod — the sweep would cover nothing", root)
	}

	var refs []colRef
	err = filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules", "bin", "rel":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		src := string(raw)
		for _, m := range sqlLiteral.FindAllStringSubmatchIndex(src, -1) {
			sql := src[m[2]:m[3]]
			line := strings.Count(src[:m[0]], "\n") + 1
			refs = append(refs, insertRefs(rel, line, sql, schema)...)
			refs = append(refs, updateRefs(rel, line, sql, schema)...)
			refs = append(refs, upsertRefs(rel, line, sql, schema)...)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return refs
}

func insertRefs(file string, line int, sql string, schema map[string]map[string]bool) []colRef {
	var out []colRef
	for _, m := range insertForm.FindAllStringSubmatch(sql, -1) {
		table := strings.ToLower(m[1])
		if schema[table] == nil {
			continue // not a migrated table (a temp/fixture table, or a parse artefact)
		}
		for _, c := range strings.Split(m[2], ",") {
			c = strings.ToLower(strings.Trim(strings.TrimSpace(c), `"`))
			if !plainIdent.MatchString(c) {
				continue
			}
			out = append(out, colRef{file, line, "INSERT", table, c})
		}
	}
	return out
}

// upsertRefs attributes the SET targets of `ON CONFLICT … DO UPDATE SET` to the
// table of the INSERT they belong to — the nearest preceding INSERT INTO in the same
// literal. Without this those assignments are invisible to the guard, and they are
// exactly the statements most likely to name a column that a later migration moved.
func upsertRefs(file string, line int, sql string, schema map[string]map[string]bool) []colRef {
	var out []colRef
	for _, loc := range upsertForm.FindAllStringSubmatchIndex(sql, -1) {
		ins := insertForm.FindAllStringSubmatchIndex(sql[:loc[0]], -1)
		if len(ins) == 0 {
			continue // a DO UPDATE with no INSERT before it in this literal
		}
		last := ins[len(ins)-1]
		table := strings.ToLower(sql[last[2]:last[3]])
		if schema[table] == nil {
			continue
		}
		for _, part := range splitTopLevel(sql[loc[2]:loc[3]]) {
			sm := setTarget.FindStringSubmatch(strings.TrimSpace(part))
			if sm == nil {
				continue
			}
			out = append(out, colRef{file, line, "UPSERT", table, strings.ToLower(sm[1])})
		}
	}
	return out
}

func updateRefs(file string, line int, sql string, schema map[string]map[string]bool) []colRef {
	var out []colRef
	for _, m := range updateForm.FindAllStringSubmatch(sql, -1) {
		table := strings.ToLower(m[1])
		if schema[table] == nil {
			continue
		}
		for _, part := range splitTopLevel(m[2]) {
			sm := setTarget.FindStringSubmatch(strings.TrimSpace(part))
			if sm == nil {
				continue
			}
			out = append(out, colRef{file, line, "UPDATE", table, strings.ToLower(sm[1])})
		}
	}
	return out
}

// splitTopLevel splits on commas that are not inside parentheses, so
// `SET a = COALESCE(x, y), b = 1` yields two assignments and not three.
func splitTopLevel(s string) []string {
	var parts []string
	depth, cur := 0, strings.Builder{}
	for _, ch := range s {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, cur.String())
				cur.Reset()
				continue
			}
		}
		cur.WriteRune(ch)
	}
	parts = append(parts, cur.String())
	return parts
}

func TestEveryWrittenColumnExistsInTheMigratedSchema(t *testing.T) {
	schema := migratedSchema(t)
	refs := writeColumnRefs(t, schema)

	if len(refs) < checkedRefFloor {
		t.Fatalf("the sweep checked %d column references, want >= %d — it has broken, and a "+
			"broken sweep reports a clean parity check", len(refs), checkedRefFloor)
	}

	var bad []string
	for _, r := range refs {
		if !schema[r.table][r.col] {
			bad = append(bad, r.file+":"+itoa(r.line)+"  "+r.form+" "+r.table+"."+r.col)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("%d column reference(s) name a column the migrated schema does not have.\n"+
			"    Each one compiles, passes review, ships, and fails at runtime on whichever code "+
			"path reaches it first:\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
	t.Logf("checked %d written column references across %d migrated relations", len(refs), len(schema))
}

// ⚠ THE GUARD'S OWN TEETH. Every assertion above is "this reference resolves", and a
// sweep that resolved nothing would satisfy all of them. This proves the resolution
// step can fail: a column that is definitely NOT in the schema must be reported.
func TestParityCheckCanActuallyFail(t *testing.T) {
	schema := migratedSchema(t)
	const table = "workspaces"
	if schema[table] == nil {
		t.Fatalf("%s is not in the migrated schema — pick another table for this control", table)
	}
	refs := insertRefs("probe.go", 1,
		"INSERT INTO "+table+" (id, zzz_no_such_column_zzz) VALUES ($1, $2)", schema)
	if len(refs) != 2 {
		t.Fatalf("the probe produced %d references, want 2 — the extractor did not read the "+
			"column list, so the check below proves nothing", len(refs))
	}
	found := false
	for _, r := range refs {
		if !schema[r.table][r.col] {
			found = true
			if r.col != "zzz_no_such_column_zzz" {
				t.Errorf("the unresolved column was %q, want the planted one", r.col)
			}
		}
	}
	if !found {
		t.Error("a column that is definitely absent from the schema resolved anyway — the parity " +
			"check cannot fail, and a guard that cannot fail is not a guard")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ⚠ THE HOLE THIS CLOSES: A MISTYPED *TABLE* NAME WAS SILENTLY SKIPPED.
// Every check above resolves a column inside a table the schema has — a reference to
// a table the schema does NOT have takes the `schema[table] == nil` branch and is
// dropped. So `INSERT INTO wokspaces (…)` would sail through the column parity check
// by never being checked at all.
//
// Measured before this was asserted: with the DO-UPDATE artefact attributed to its
// enclosing INSERT (see upsertRefs), EVERY INSERT/UPDATE target table in the
// repository's Go SQL is in the migrated schema. Requiring it therefore costs no
// false positives.
func TestEveryWrittenTableExistsInTheMigratedSchema(t *testing.T) {
	schema := migratedSchema(t)
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	type site struct {
		file  string
		line  int
		verb  string
		table string
	}
	var targets []site
	err = filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules", "bin", "rel":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		src := string(raw)
		for _, m := range sqlLiteral.FindAllStringSubmatchIndex(src, -1) {
			sql := src[m[2]:m[3]]
			line := strings.Count(src[:m[0]], "\n") + 1
			for _, im := range insertForm.FindAllStringSubmatch(sql, -1) {
				targets = append(targets, site{rel, line, "INSERT INTO", strings.ToLower(im[1])})
			}
			for _, um := range updateForm.FindAllStringSubmatch(sql, -1) {
				targets = append(targets, site{rel, line, "UPDATE", strings.ToLower(um[1])})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if len(targets) < writeTargetFloor {
		t.Fatalf("found %d INSERT/UPDATE target tables, want >= %d — the sweep has broken",
			len(targets), writeTargetFloor)
	}

	var bad []string
	for _, s := range targets {
		if schema[s.table] == nil {
			bad = append(bad, s.file+":"+itoa(s.line)+"  "+s.verb+" "+s.table)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("%d statement(s) write to a table the migrated schema does not have.\n"+
			"    A wrong TABLE name is not caught by the column parity check — that check can only "+
			"resolve columns inside tables it knows, so an unknown table is skipped entirely:\n  %s",
			len(bad), strings.Join(bad, "\n  "))
	}
	t.Logf("checked %d INSERT/UPDATE target tables", len(targets))
}
