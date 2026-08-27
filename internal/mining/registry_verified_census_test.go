package mining

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// registry_verified_census_test.go — every SQL statement in this repository that
// names a node-registry table, and what each one does with `verified`.
//
// THE MEASUREMENT THIS PINS, taken 2026-08-27 and reproduced by this test on every
// run: `verified` is the node registry's only "is this node real" flag, and
//
//	inference_nodes  1 writer, 1 gate, 2 projections
//	embedding_nodes  1 writer, 1 gate, 2 projections
//	cache_nodes      0 writers, 0 gates, 0 projections
//
// The single gate on each of the first two is ListAvailableNodes — the query
// behind the ANONYMOUS discovery route (see cmd/lens/public_node_discovery.go),
// which appears in no SDK, no document and no other Talyvor repository. So the
// registry's only trust flag currently gates one listing nobody calls, and on the
// third registry table it is written by nothing, read by nothing, and therefore
// FALSE for every row that has ever existed — `cache_nodes.verified` takes its
// DEFAULT FALSE at INSERT (cmd/lens/main.go) and no statement anywhere updates it.
//
// ⚠ WHY THIS IS A TEST AND NOT A NOTE. What `verified` MEANS is measured next door
// in verified_flag_meaning_realpg_test.go: something at the registered URL
// answered HTTP 200 to an unauthenticated GET. A flag that reads as trust and
// carries that little is harmless while nothing trusts it and dangerous the moment
// something does. This test fires when the set of things that trust it changes —
// in either direction — so that change is a decision somebody makes rather than a
// line somebody adds.
//
// ⚠ NOT THE SAME `verified` AS mint_gate.go's. ErrEarnNotVerified / the U6 Sybil
// floor is a WORKSPACE-level verification and has nothing to do with these
// columns. Nor is node_attestations.attestation_status = 'verified', which is a
// string value on a different table — the classifier below deliberately excludes
// quoted 'verified' for exactly that reason, and TestRegistryCensusClassifier-
// DoesNotCountQuotedVerified is the control proving it does.

var registryTables = []string{"inference_nodes", "embedding_nodes", "cache_nodes"}

// bareVerified matches the COLUMN `verified`, never the string literal 'verified'.
var (
	quotedVerified = regexp.MustCompile(`'verified'`)
	setVerified    = regexp.MustCompile(`(?i)\bSET\s+verified\b`)
	whereVerified  = regexp.MustCompile(`(?is)\bWHERE\b.*?\bverified\b`)
	anyVerified    = regexp.MustCompile(`\bverified\b`)
	sqlLiteral     = regexp.MustCompile("`([^`]*)`")
)

type registryStmt struct {
	file   string
	tables []string
	class  string // "write" | "gate" | "project" | "none"
}

// repoRoot resolves the module root from this package's directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod — the sweep below would cover nothing: %v", root, err)
	}
	return root
}

// classify decides what one SQL literal does with the registry's `verified`
// column. Quoted 'verified' is stripped first so a status value on another table
// cannot be counted as a gate on this one.
func classify(sql string) string {
	clean := quotedVerified.ReplaceAllString(sql, "'__STATUSVALUE__'")
	switch {
	case setVerified.MatchString(clean):
		return "write"
	case whereVerified.MatchString(clean):
		return "gate"
	case anyVerified.MatchString(clean):
		return "project"
	default:
		return "none"
	}
}

func sweepRegistryStatements(t *testing.T) []registryStmt {
	t.Helper()
	root := repoRoot(t)
	var out []registryStmt

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
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
		for _, m := range sqlLiteral.FindAllStringSubmatch(string(raw), -1) {
			sql := m[1]
			var named []string
			for _, tbl := range registryTables {
				if regexp.MustCompile(`\b` + tbl + `\b`).MatchString(sql) {
					named = append(named, tbl)
				}
			}
			if len(named) == 0 {
				continue
			}
			out = append(out, registryStmt{file: rel, tables: named, class: classify(sql)})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return out
}

// countsByTable returns table → class → n, over statements naming that table.
func countsByTable(stmts []registryStmt) map[string]map[string]int {
	out := map[string]map[string]int{}
	for _, tbl := range registryTables {
		out[tbl] = map[string]int{}
	}
	for _, s := range stmts {
		for _, tbl := range s.tables {
			out[tbl][s.class]++
		}
	}
	return out
}

// registryStatementFloor is the non-vacuity floor. The sweep found 38 SQL
// literals naming a registry table when this was written; a sweep that suddenly
// finds a handful has broken, and a broken sweep reports every expectation below
// as satisfied.
const registryStatementFloor = 30

func TestRegistryVerifiedCensus(t *testing.T) {
	stmts := sweepRegistryStatements(t)
	if len(stmts) < registryStatementFloor {
		t.Fatalf("the sweep found only %d SQL literals naming a node-registry table, want >= %d — "+
			"the walk is broken, and a broken walk satisfies every count below",
			len(stmts), registryStatementFloor)
	}

	got := countsByTable(stmts)
	want := map[string]map[string]int{
		"inference_nodes": {"write": 1, "gate": 1, "project": 2},
		"embedding_nodes": {"write": 1, "gate": 1, "project": 2},
		// ⚠ THE ROW THAT IS THE FINDING. cache_nodes carries the same
		// `verified BOOLEAN NOT NULL DEFAULT FALSE` column as its two siblings
		// (migrations/0026), the INSERT omits it, and NOTHING anywhere writes,
		// gates on, or even selects it. Every cache_nodes row is verified=FALSE
		// and always has been. The cache mint that would have consumed it was
		// retired ("Phase-4a Item 0 — superseded by pool-royalty", main.go), and
		// the column outlived it.
		"cache_nodes": {"write": 0, "gate": 0, "project": 0},
	}

	for _, tbl := range registryTables {
		for _, class := range []string{"write", "gate", "project"} {
			if got[tbl][class] != want[tbl][class] {
				t.Errorf("%s: %d statement(s) %s `verified`, want %d.\n"+
					"    What `verified` proves is measured in verified_flag_meaning_realpg_test.go: "+
					"that something at the registered URL answered HTTP 200 to an unauthenticated GET. "+
					"Changing who trusts it is a decision — take it deliberately and update this "+
					"census with the reason, do not just move the number.",
					tbl, got[tbl][class], class+"s on", want[tbl][class])
			}
		}
	}

	if t.Failed() {
		var lines []string
		for _, s := range stmts {
			if s.class != "none" {
				lines = append(lines, s.file+" ["+strings.Join(s.tables, ",")+"] "+s.class)
			}
		}
		sort.Strings(lines)
		t.Logf("statements touching `verified`:\n  %s", strings.Join(lines, "\n  "))
	}
}

// CONTROL for the classifier itself, kept as a test rather than left to the
// mutation harness because it guards a specific false positive that was actually
// made: node_attestations.attestation_status = 'verified' appears inside a
// statement that JOINs inference_nodes (internal/poolroyalty/confidential_minter.go).
// A naive `\bverified\b` inside a WHERE counts that as a gate on the registry
// column, and the census then reports inference_nodes with two gates instead of
// one — a number that looks safer than the truth.
func TestRegistryCensusClassifierDoesNotCountQuotedVerified(t *testing.T) {
	const attestationJoin = `WITH valid_attestation AS (
	    SELECT DISTINCT node_id FROM node_attestations
	    WHERE attestation_status = 'verified' AND key_bound = true)
	SELECT a.node_id, n.workspace_id FROM valid_attestation a
	JOIN inference_nodes n ON n.id = a.node_id`
	if c := classify(attestationJoin); c != "none" {
		t.Errorf("classify(attestation join) = %q, want \"none\" — a status VALUE named "+
			"'verified' on another table was counted as a gate on the registry column", c)
	}

	// And the mirror: a genuine gate must still be seen, so the exclusion above is
	// not simply blinding the classifier.
	const realGate = `SELECT id, workspace_id, url FROM inference_nodes
	    WHERE verified = TRUE AND active = TRUE AND $1 = ANY(models)`
	if c := classify(realGate); c != "gate" {
		t.Errorf("classify(real gate) = %q, want \"gate\" — the quoted-literal exclusion has "+
			"blinded the classifier to the thing it exists to count", c)
	}
	const realWrite = `UPDATE inference_nodes SET verified = TRUE WHERE id = $1`
	if c := classify(realWrite); c != "write" {
		t.Errorf("classify(real write) = %q, want \"write\"", c)
	}
	const projection = `SELECT id, workspace_id, url, active, verified, created_at
	    FROM inference_nodes WHERE workspace_id = $1`
	if c := classify(projection); c != "gate" && c != "project" {
		t.Errorf("classify(projection) = %q, want a verified-touching class", c)
	}
}
