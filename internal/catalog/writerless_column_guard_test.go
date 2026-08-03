package catalog

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// NO SQL MAY PREDICATE ON A COLUMN NOTHING WRITES.
//
// ⚠ THIS DEFECT HAS NOW BEEN WRITTEN TWICE. `token_events.cached` is BOOLEAN NOT NULL DEFAULT
// false and has never had a writer — not one INSERT names it, no UPDATE touches it. Two separate
// surfaces read it as though it meant something:
//
//	internal/api/server.go   — reported a structural 0 as a measured cache hit rate. Fixed by
//	                           migration 0100 (serve_source), and the fix is documented there.
//	internal/mcp/server.go   — the SAME defect, left behind by that fix, so `get_cache_stats`
//	                           answered estimated_savings_usd = $0.00 for a year. Fixed here.
//
// Fixing an instance does not stop the next one. The column is still in the schema, so the next
// person to write an analytics query sees `cached` in `\d token_events`, uses it, and reports
// another confident zero. A guard is the only thing that makes a third instance impossible, and
// it is cheap: these columns are enumerated, not inferred.
//
// ⚠ WHY THIS TEST AND NOT A DROP. Dropping the column would make the trap unrepresentable, which
// is normally the stronger fix — but `token_events` is a partitioned hot table (migration 0034)
// and the repo has no down-migrations. An irreversible ACCESS EXCLUSIVE schema change on the
// billing table has a completely different risk profile from correcting a query, and bundling the
// two means a rollback of the query fix cannot roll back the schema. The recommendation to drop
// stands; it belongs in its own change. Until then this guard carries the knowledge, and
// migration 0114 puts the same warning where a DBA with psql will actually see it.
//
// If a column here is ever given a real writer, delete its entry — the guard is a statement about
// the CURRENT writers, not a permanent ban.
var writerlessColumns = []string{
	// token_events — all three from 0001_init.sql, none ever written.
	"cached",
	"compressed",
	"savings_pct",
}

// predicateUse matches a column being used as a PREDICATE or an aggregate subject in SQL — the
// uses that silently produce a wrong number. A bare appearance in a column list is not matched:
// `SELECT ... , cached, ...` returns a false that is at least honestly false.
func predicateUse(col string) *regexp.Regexp {
	c := regexp.QuoteMeta(col)
	return regexp.MustCompile(`(?i)FILTER\s*\(\s*WHERE\s+(NOT\s+)?` + c + `\s*\)` +
		`|WHERE\s+(NOT\s+)?` + c + `\s*(=|<|>|IS|AND|OR|\)|$)` +
		`|AND\s+(NOT\s+)?` + c + `\s*(=|<|>|IS|AND|OR|\)|$)` +
		`|SUM\s*\(\s*` + c + `\s*\)|AVG\s*\(\s*` + c + `\s*\)`)
}

func TestNoSQLPredicatesOnAWriterlessColumn(t *testing.T) {
	root := "../.."
	res := make(map[string]*regexp.Regexp, len(writerlessColumns))
	for _, c := range writerlessColumns {
		res[c] = predicateUse(c)
	}

	var offences []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "migrations":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue // the fix's own explanation quotes the broken SQL
			}
			for col, re := range res {
				if re.MatchString(line) {
					offences = append(offences, filepath_rel(root, path)+":"+itoa(i+1)+"  ["+col+"]  "+trimmed)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offences) > 0 {
		t.Fatalf("SQL predicates on column(s) that NOTHING WRITES — every row has the default, so "+
			"this query's answer is a constant reported as a measurement:\n  %s\n\n"+
			"This is the third occurrence of a defect that has already shipped twice (api/server.go, "+
			"then mcp/server.go). If the column now has a real writer, remove it from "+
			"writerlessColumns above and say where the writer is.",
			strings.Join(offences, "\n  "))
	}
}

// ⚠ POSITIVE CONTROL ON THE GUARD ITSELF. A pattern-matcher that matches nothing passes silently
// and forever; this proves it can still see the exact SQL that shipped.
func TestWriterlessColumnGuardCanSeeTheShippedDefect(t *testing.T) {
	shipped := []string{
		"  COUNT(*) FILTER (WHERE cached)",
		"  COALESCE(SUM(cost_usd) FILTER (WHERE NOT cached), 0)",
	}
	re := predicateUse("cached")
	for _, line := range shipped {
		if !re.MatchString(line) {
			t.Errorf("the guard does NOT match a line that really shipped: %q — it would have let "+
				"both known instances through", line)
		}
	}
	// And it must not fire on an honest column-list mention, or it is unusable.
	for _, ok := range []string{
		"  id, provider, model, input_tokens, output_tokens, cached, compressed,",
		"\t\t&s.NodeID, &s.RequestsServed,",
	} {
		if re.MatchString(ok) {
			t.Errorf("the guard fires on a benign line, which would force it to be disabled: %q", ok)
		}
	}
}

// tiny helpers, kept local so the guard has no dependency that could drift
func filepath_rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
