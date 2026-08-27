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
)

// orphan_relation_test.go — the MIRROR of the parity guard next door.
//
// schema_sql_parity_test.go (W6.21, #489) proves every column this binary WRITES
// exists in the migrated schema. This asks the quieter question in the other
// direction: which relations does the schema HAVE that the binary never touches at
// all? That is the "captured field with no join key" class — storage, migration risk
// and a privacy surface with no product behind it, and nothing here would ever have
// said so.
//
// ⚠ THE MEASUREMENT IS TRUSTWORTHY FOR ONE CHECKABLE REASON: this repository contains
// ZERO `SELECT *` in non-test Go, asserted below. A column or table can otherwise be
// read without ever being named, and the whole question would be unanswerable by
// searching text.
//
// ⚠ AND TWO EXCLUSIONS, BOTH MECHANICAL RATHER THAN CURATED:
//   - PARTITIONS. Postgres routes writes on the parent to its partitions, so code
//     never names `token_events_p3`. Excluded via pg_class.relispartition, not by a
//     name rule — 24 of the 125 relations, and a name rule would be a guess.
//   - TABLE NAMES PASSED AS GO STRING CONSTANTS. poolroyalty's writers are
//     parameterised by table (NewAdjudicationWriterForTable(pool, rev,
//     "distill_royalty_adjudications")), so the name never appears inside a SQL
//     literal. A sweep of literals alone calls those orphans, and they are not.

var goStringLit = regexp.MustCompile(`"([a-z_][a-z0-9_]{2,60})"`)

// orphanRelations — a relation the binary never names in a SQL literal, with why.
// Each entry is a claim about the repository that this test re-checks on every run.
var orphanRelations = map[string]string{
	"ab_tests": "ORPHAN. ⚠ AND migrations/0016_experiments.sql SAYS OTHERWISE IN ITS OWN " +
		"HEADER: \"Coexists with ab_tests (the original shadow-A/B table) which keeps the proxy " +
		"hot-path shadow-probe contract working unchanged.\" No SQL literal and no Go string " +
		"names it, so no hot-path contract reads or writes it. ⚠ And internal/attribution/" +
		"tracker.go already calls it \"the ab_tests precedent, #154\" for a table left orphaned " +
		"without a drop migration — two files in this tree describe it in contradictory ways and " +
		"the sweep settles which is true.",
	"branch_spend": "ORPHAN, and accurately documented as one: internal/attribution/tracker.go " +
		"records that the double-write was retired in #157 and the reads moved to " +
		"request_attribution in #158, \"leaving branch_spend with no reader … left orphaned (no " +
		"drop migration; drop later once confirmed empty in deploys)\". The sweep agrees.",
}

// namedOnlyByGoString — relations whose name reaches SQL through a Go string constant
// rather than a literal. Recorded separately because two of them are genuinely in use
// and one is not, and collapsing that distinction would be the flattering error.
var namedOnlyByGoString = map[string]string{
	"distill_royalty_adjudications": "IN USE. poolroyalty.NewAdjudicationWriterForTable is " +
		"parameterised by table name, so the name is a Go constant and never appears in a literal.",
	"pool_royalty_adjudications": "IN USE, same parameterised writer.",
	"guardrail_events": "⚠ NOT IN USE. Its ONLY mention in non-test Go is " +
		"internal/tenantdata/manifest.go, which lists it for deletion when a tenant is erased — " +
		"so the tenant-deletion manifest promises to delete rows from a table NOTHING WRITES. " +
		"The guardrails engine persists to guardrail_policies and nothing else.",
}

func nonPartitionRelations(t *testing.T) []string {
	t.Helper()
	adminURL := os.Getenv("LENS_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping orphan-relation census")
	}
	_ = migratedSchema(t) // builds (and cleans up) the migrated database
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, swapDatabase(adminURL, schemaCheckDB))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, `SELECT c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r','p','v','m') AND c.relispartition = false
		ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("list relations: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, n)
	}
	return out
}

// sweepNames returns the identifiers appearing in SQL literals and the Go string
// constants, over every non-test .go file.
func sweepNames(t *testing.T) (inSQL, inGoString map[string]bool, selectStars []string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	inSQL, inGoString = map[string]bool{}, map[string]bool{}
	sqlish := regexp.MustCompile(`(?i)\b(SELECT|INSERT|UPDATE|DELETE|RETURNING|ON CONFLICT|TRUNCATE)\b`)
	ident := regexp.MustCompile(`[a-z_][a-z0-9_]*`)
	star := regexp.MustCompile(`(?i)SELECT\s+\*`)

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
		for _, m := range sqlLiteral.FindAllStringSubmatch(src, -1) {
			if !sqlish.MatchString(m[1]) {
				continue
			}
			if star.MatchString(m[1]) {
				selectStars = append(selectStars, rel)
			}
			for _, id := range ident.FindAllString(strings.ToLower(m[1]), -1) {
				inSQL[id] = true
			}
		}
		for _, m := range goStringLit.FindAllStringSubmatch(src, -1) {
			inGoString[strings.ToLower(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return inSQL, inGoString, selectStars
}

// ⚠ THE PREMISE OF THE WHOLE CENSUS. If a `SELECT *` appears, a column can be read
// without being named and every "unread" claim below becomes unfalsifiable.
func TestNoSelectStarMakesTheCensusAnswerable(t *testing.T) {
	_, _, stars := sweepNames(t)
	if len(stars) > 0 {
		sort.Strings(stars)
		t.Errorf("`SELECT *` appears in %d non-test file(s): %s\n"+
			"    A star reads columns without naming them, so the orphan census below can no "+
			"longer tell 'nothing reads this' from 'something reads it invisibly'. Either name "+
			"the columns or record why the census is still answerable.",
			len(stars), strings.Join(stars, ", "))
	}
}

func TestOrphanRelationCensus(t *testing.T) {
	rels := nonPartitionRelations(t)
	if len(rels) < 90 {
		t.Fatalf("found %d non-partition relations, want >= 90 — the schema read has broken, and "+
			"an empty list reports no orphans", len(rels))
	}
	inSQL, inGoString, _ := sweepNames(t)
	if len(inSQL) < 500 {
		t.Fatalf("the SQL sweep collected %d identifiers, want >= 500 — it has broken, and an "+
			"empty sweep calls every relation an orphan", len(inSQL))
	}

	var neverNamed, onlyGoString []string
	for _, r := range rels {
		low := strings.ToLower(r)
		if inSQL[low] {
			continue
		}
		if inGoString[low] {
			onlyGoString = append(onlyGoString, r)
			continue
		}
		neverNamed = append(neverNamed, r)
	}

	check := func(got []string, want map[string]string, what string) {
		sort.Strings(got)
		gotSet := map[string]bool{}
		for _, g := range got {
			gotSet[g] = true
			if _, ok := want[g]; !ok {
				t.Errorf("%s: %s is not in W6.22's record.\n"+
					"    A relation the migrated schema has and this binary never touches is "+
					"storage, migration risk and a privacy surface with no product behind it. Say "+
					"what it is for, or drop it.", what, g)
			}
		}
		for w := range want {
			if !gotSet[w] {
				t.Errorf("%s: W6.22's record names %s and the sweep no longer finds it there. If "+
					"it came into use, or was dropped, the record is stale — update it with what "+
					"changed.", what, w)
			}
		}
	}
	check(neverNamed, orphanRelations, "named nowhere")
	check(onlyGoString, namedOnlyByGoString, "named only by a Go string constant")

	t.Logf("%d non-partition relations: %d named nowhere (%s), %d named only by a Go string (%s)",
		len(rels), len(neverNamed), strings.Join(neverNamed, ", "),
		len(onlyGoString), strings.Join(onlyGoString, ", "))
}

// ⚠ THE CENSUS'S TEETH. Every assertion above is "the orphan set is exactly this";
// a sweep that found every relation named would report an empty set and satisfy
// nothing. This proves a relation CAN be classified as an orphan.
func TestOrphanCensusCanActuallyClassify(t *testing.T) {
	inSQL, inGoString, _ := sweepNames(t)
	const invented = "zzz_no_such_relation_zzz"
	if inSQL[invented] || inGoString[invented] {
		t.Fatalf("%s appears in the tree — pick another name for this control", invented)
	}
	// A real, heavily-used table must be seen as named, or the sweep is blind rather
	// than the relation orphaned.
	for _, used := range []string{"token_events", "workspaces", "inference_nodes"} {
		if !inSQL[used] {
			t.Errorf("%s is not seen in any SQL literal — the sweep is blind, and a blind sweep "+
				"calls everything an orphan", used)
		}
	}
}
