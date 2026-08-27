package catalog

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// THE GUARD NEXT DOOR COULD NOT SEE THE FOURTH INSTANCE, AND MIGRATION 0114 SAYS IT COULD.
//
// writerless_column_guard_test.go carries the knowledge that token_events has columns nothing
// writes, and migration 0114 states it "fails the build if a fourth appears". A fourth appeared —
// token_events.prompt_hash, censused: the only two production statements that write token_events
// (internal/alerts insertTokenEventSQL, insertCacheServeSQL) name neither it nor anything like it,
// so every row carries the NOT NULL DEFAULT of the empty string. It shipped a wrong customer-facing
// number through internal/learner analyseSQL for as long as that query has existed. The guard
// missed it for TWO INDEPENDENT REASONS, and both had to be fixed:
//
//	1. writerlessColumns is a HAND-ENUMERATED list. It cannot discover a fourth column; it can only
//	   re-check the three someone already wrote down. "Fails the build if a fourth appears" was
//	   never true of it.
//	2. predicateUse matches FILTER / WHERE / AND / SUM / AVG. The two real uses of prompt_hash are
//	   GROUP BY (internal/learner) and JOIN ... ON (internal/warmer) — neither is in that set.
//
// ⚠ AND THE OBVIOUS FIX IS WRONG, WHICH IS WHY THIS IS A SEPARATE GUARD RATHER THAN THREE MORE
// LINES IN THE OTHER ONE. `prompt_hash` is NOT a unique column name. prompt_embeddings.prompt_hash
// is a sha256 that IS written on every semantic-cache store, and internal/warmer's candidate query
// legitimately groups by it. A name-only matcher — which is all the neighbouring guard is — flags
// that legitimate use and would have to be switched off. So this guard resolves the TABLE: it reads
// each SQL literal, works out which aliases are bound to token_events, and only complains about
// uses that resolve to token_events.
//
// ⚠ ONE OFFENCE IS KNOWN, MEASURED, AND DELIBERATELY NOT FIXED HERE — see knownOffences below.

// writerlessQualified are columns that are writerless ON token_events but whose NAME may legally
// appear on other tables. Entries here are matched only when they resolve to token_events.
//
// If one of these ever gains a real writer, delete its entry — this is a statement about the
// CURRENT writers, not a permanent ban. internal/learner/writerless_prompt_hash_test.go censuses
// the writers and fails the build the moment that happens, so the two cannot drift apart.
var writerlessQualified = []string{
	"prompt_hash",
}

// knownOffences are the exact normalised SQL uses that exist TODAY and are not fixable without a
// decision. Keyed on the offending text, NOT on the file — a file-level exemption would swallow
// the next, unrelated offence in the same file, which is the failure mode that makes allowlists
// worse than no guard.
//
// Each entry must state what was measured and what decision it waits on.
var knownOffences = map[string]string{
	// internal/warmer/warmer.go candidatesSQL.
	//
	// MEASURED on real Postgres over the full migration chain, with a positive control: seed
	// prompt_embeddings with a popular row keyed exactly as the production semantic-cache writer
	// keys it, and GetWarmCandidates returns 0 candidates; insert ONE token_events row carrying
	// that same hash — the value no production writer produces — and the same query returns 1.
	// The cache warmer has therefore never had a candidate and cannot have one.
	//
	// NOT FIXED HERE BECAUSE EVERY REPAIR IS A DECISION, AND EACH ONE SPENDS MONEY:
	//   · giving token_events.prompt_hash a writer means retaining a per-prompt fingerprint under
	//     the DEFAULT `metadata` logging policy, whose stated purpose is to strip prompt identity;
	//   · the prompt_text this join exists to recover is itself the empty string under that same
	//     default policy (internal/proxy sets spendPrompt to empty for LoggingMetadata), and
	//     WarmOne has no empty-prompt refusal — measured, it POSTs
	//     {"messages":[{"content":"","role":"user"}]} to the provider on the OPERATOR's key and
	//     reports success;
	//   · and the warmer writes its result under the BARE prompt key while every production read
	//     is workspace-prefixed or pooled-marked, so the entry it pays for cannot be hit.
	// The warmer is inert three independent ways and is, by its own wiring comment, "the ONLY
	// background job in this binary that spends money at an external provider".
	"JOIN token_events te ON te.prompt_hash = pe.prompt_hash": "internal/warmer candidatesSQL — " +
		"the cache warmer's join key. Measured inert (0 candidates, positive-controlled). Repair " +
		"is a privacy + money decision, reported to the queue under W4.9; do not silence, fix.",
}

// aliasesForTokenEvents returns every name a SQL statement uses to refer to token_events: the bare
// table name plus any alias bound to it (`token_events te`, `token_events AS te`).
func aliasesForTokenEvents(sql string) []string {
	out := []string{"token_events"}
	re := regexp.MustCompile(`(?i)token_events\s+(?:AS\s+)?([a-z_][a-z0-9_]*)`)
	for _, m := range re.FindAllStringSubmatch(sql, -1) {
		switch strings.ToUpper(m[1]) {
		// Not aliases — the next SQL keyword.
		case "ON", "WHERE", "GROUP", "ORDER", "HAVING", "LIMIT", "JOIN", "LEFT", "RIGHT", "INNER",
			"OUTER", "SET", "VALUES", "USING", "RETURNING", "AS", "FULL", "CROSS", "UNION":
			continue
		}
		out = append(out, m[1])
	}
	return out
}

// excludesTheDefault reports whether a statement explicitly filters OUT the writerless column's
// default value. That distinction is the whole point of this guard rather than a detail of it.
//
// The defect being hunted is a query whose answer is A CONSTANT REPORTED AS A MEASUREMENT: every
// row carries the default, so the predicate is inert or the grouping collapses, and something
// wrong gets rendered as a number. A statement that says `prompt_hash <> ”` has the opposite
// property — it can only ever return rows the column genuinely distinguishes, so today it returns
// NOTHING and the instant a writer exists it returns the right thing, with no code change. That is
// the sanctioned shape, and recognising it here is what lets the fix in internal/learner stay
// self-correcting instead of being frozen into an allowlist.
//
// ⚠ It must be the COLUMN's own emptiness test, not any mention of it — see the positive controls,
// which check that the warmer's join (no such test) is still flagged.
func excludesTheDefault(flat, name string) bool {
	c := regexp.QuoteMeta(name)
	return regexp.MustCompile(`(?i)` +
		c + `\s*(<>|!=)\s*''` +
		`|` + c + `\s+IS\s+DISTINCT\s+FROM\s+''` +
		`|length\s*\(\s*` + c + `\s*\)\s*>\s*0` +
		`|NULLIF\s*\(\s*` + c + `\s*,\s*''\s*\)\s+IS\s+NOT\s+NULL`).MatchString(flat)
}

// qualifiedUse matches the ways a column actually gets read as a decision: predicates, aggregates,
// GROUP BY, and JOIN ... ON. `name` is either the bare column (for a statement whose only table is
// token_events) or an alias-qualified `te.col`.
func qualifiedUse(name string) *regexp.Regexp {
	c := regexp.QuoteMeta(name)
	return regexp.MustCompile(`(?i)` +
		`FILTER\s*\(\s*WHERE\s+(NOT\s+)?` + c + `\b` +
		`|WHERE\s+(NOT\s+)?` + c + `\s*(=|<>|!=|<|>|IS|LIKE|AND|OR|\))` +
		`|AND\s+(NOT\s+)?` + c + `\s*(=|<>|!=|<|>|IS|LIKE|AND|OR|\))` +
		`|SUM\s*\(\s*` + c + `\s*\)|AVG\s*\(\s*` + c + `\s*\)|COUNT\s*\(\s*DISTINCT\s+` + c + `\s*\)` +
		`|GROUP\s+BY\s+(?:[a-z0-9_.]+\s*,\s*)*` + c + `\b` +
		`|ON\s+[a-z0-9_.]*\s*=?\s*` + c + `\s*=` +
		`|ON\s+` + c + `\s*=` +
		`|=\s*` + c + `\b`)
}

// sqlLiterals pulls the backtick-quoted raw strings out of a Go file — this repo writes every SQL
// statement as one. Returned with their 1-indexed starting line so an offence can be located.
func sqlLiterals(src string) []struct {
	text string
	line int
} {
	var out []struct {
		text string
		line int
	}
	for i := 0; i < len(src); i++ {
		if src[i] != '`' {
			continue
		}
		j := strings.IndexByte(src[i+1:], '`')
		if j < 0 {
			break
		}
		lit := src[i+1 : i+1+j]
		if strings.Contains(strings.ToLower(lit), "token_events") {
			out = append(out, struct {
				text string
				line int
			}{lit, strings.Count(src[:i], "\n") + 1})
		}
		i = i + 1 + j
	}
	return out
}

func TestNoTableQualifiedUseOfAWriterlessTokenEventsColumn(t *testing.T) {
	root := "../.."
	var offences []string
	seenKnown := map[string]bool{}

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
		// Production Go only: tests seed these columns by hand precisely because production
		// does not, and this guard's own fixtures below would otherwise flag themselves.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, lit := range sqlLiterals(string(b)) {
			flat := strings.Join(strings.Fields(lit.text), " ")
			for _, col := range writerlessQualified {
				var names []string
				for _, a := range aliasesForTokenEvents(lit.text) {
					if a == "token_events" {
						// Bare column names are ambiguous unless token_events is the only
						// table in the statement — otherwise the name may belong to the
						// other table, as prompt_embeddings.prompt_hash legitimately does.
						if onlyTable(lit.text) {
							names = append(names, col)
						}
						continue
					}
					names = append(names, a+"."+col)
				}
				for _, n := range names {
					if !qualifiedUse(n).MatchString(flat) {
						continue
					}
					if excludesTheDefault(flat, n) {
						continue // the sanctioned self-correcting shape; see excludesTheDefault
					}
					if reason, ok := matchKnown(flat); ok {
						seenKnown[reason] = true
						continue
					}
					offences = append(offences, filepath_rel(root, path)+":"+itoa(lit.line)+
						"  ["+n+"]  "+truncate(flat, 160))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offences) > 0 {
		t.Fatalf("SQL that decides on a token_events column NOTHING WRITES — every row carries the "+
			"default, so the answer is a constant reported as a measurement:\n  %s\n\n"+
			"If the column now has a real writer, remove it from writerlessQualified and say where "+
			"the writer is. If it does not, this query is inert and must be fixed or recorded in "+
			"knownOffences with what was measured.", strings.Join(offences, "\n  "))
	}

	// AN ALLOWLIST THAT OUTLIVES ITS SUBJECT IS A LIE. If a known offence is gone, this guard must
	// say so rather than keep excusing something that no longer exists.
	for frag, reason := range knownOffences {
		if !seenKnown[reason] {
			t.Errorf("knownOffences still excuses a use that is no longer in the tree: %q\n  "+
				"(%s)\n  It was either fixed — delete the entry — or moved, in which case the "+
				"guard is no longer watching it.", frag, reason)
		}
	}
}

func onlyTable(sql string) bool {
	l := strings.ToLower(sql)
	for _, other := range []string{"prompt_embeddings", "lxc_ledger", "lens_token_ledger",
		"request_attribution", "workspaces", "prompt_", "batch_jobs"} {
		if other != "prompt_" && strings.Contains(l, other) {
			return false
		}
	}
	return true
}

func matchKnown(flat string) (string, bool) {
	for frag, reason := range knownOffences {
		if strings.Contains(flat, frag) {
			return reason, true
		}
	}
	return "", false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ⚠ POSITIVE CONTROL. A matcher that matches nothing passes forever. These are the two REAL
// statements — the one just fixed and the one still open — and the legitimate uses that must not
// be flagged, because a guard that fires on correct code gets deleted.
func TestQualifiedGuardSeesBothRealUsesAndNeitherLegitimateOne(t *testing.T) {
	mustFlag := []struct{ name, sql string }{
		{
			// internal/learner analyseSQL as it shipped, before the fix.
			name: "learner GROUP BY (fixed in 439dca9)",
			sql: "SELECT prompt_hash, COUNT(*) as hit_count FROM token_events " +
				"WHERE created_at > NOW() - INTERVAL '7 days' GROUP BY prompt_hash HAVING COUNT(*) >= 3",
		},
		{
			// internal/warmer candidatesSQL as it stands today.
			name: "warmer JOIN ... ON (open)",
			sql: "SELECT pe.prompt_hash, pe.provider FROM prompt_embeddings pe " +
				"JOIN token_events te ON te.prompt_hash = pe.prompt_hash WHERE pe.hit_count >= 5",
		},
	}
	for _, c := range mustFlag {
		flat := strings.Join(strings.Fields(c.sql), " ")
		var hit bool
		for _, a := range aliasesForTokenEvents(c.sql) {
			n := a + ".prompt_hash"
			if a == "token_events" {
				if !onlyTable(c.sql) {
					continue
				}
				n = "prompt_hash"
			}
			if qualifiedUse(n).MatchString(flat) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("guard does NOT see a use that really shipped (%s): %q — it would have let "+
				"the defect through exactly as the name-only guard did", c.name, c.sql)
		}
	}

	mustNotFlag := []struct{ name, sql string }{
		{
			// prompt_embeddings.prompt_hash IS written on every semantic-cache store. Flagging it
			// would make this guard unusable and it would be switched off.
			name: "grouping the WRITTEN prompt_embeddings.prompt_hash",
			sql: "SELECT pe.prompt_hash FROM prompt_embeddings pe JOIN token_events te " +
				"ON te.request_id = pe.request_id GROUP BY pe.prompt_hash, pe.provider",
		},
		{
			name: "a column-list mention, which is at least honestly empty",
			sql:  "SELECT workspace_id, prompt_hash, cost_usd FROM token_events WHERE workspace_id = $1",
		},
	}
	for _, c := range mustNotFlag {
		flat := strings.Join(strings.Fields(c.sql), " ")
		for _, a := range aliasesForTokenEvents(c.sql) {
			n := a + ".prompt_hash"
			if a == "token_events" {
				if !onlyTable(c.sql) {
					continue
				}
				n = "prompt_hash"
			}
			if qualifiedUse(n).MatchString(flat) {
				t.Errorf("guard fires on a legitimate use (%s) via %q: %q — a guard that fires on "+
					"correct code gets disabled, which is worse than no guard", c.name, n, c.sql)
			}
		}
	}
}

// ⚠ POSITIVE CONTROL ON THE ALIAS RESOLVER, which is the part that makes the rest correct.
func TestAliasResolutionFindsTheTokenEventsAliasAndNotAKeyword(t *testing.T) {
	got := aliasesForTokenEvents("FROM prompt_embeddings pe JOIN token_events te ON te.x = pe.x")
	if !contains(got, "te") {
		t.Errorf("alias resolver missed `te`: %v — every qualified match depends on this", got)
	}
	got2 := aliasesForTokenEvents("FROM token_events WHERE created_at > NOW()")
	if contains(got2, "WHERE") || contains(got2, "where") {
		t.Errorf("alias resolver treated a keyword as an alias: %v", got2)
	}
	got3 := aliasesForTokenEvents("FROM token_events AS ev GROUP BY ev.prompt_hash")
	if !contains(got3, "ev") {
		t.Errorf("alias resolver missed an AS-form alias: %v", got3)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ⚠ POSITIVE CONTROL ON THE EXEMPTION, which is the only way this guard can be silently defeated.
// excludesTheDefault suppresses an offence, so if it were too generous the whole guard would pass
// on everything and look healthy. These pin exactly what it does and does not accept.
func TestTheEmptinessExemptionAcceptsOnlyTheColumnsOwnTest(t *testing.T) {
	accept := []struct{ name, sql string }{
		{"the shipped fix in internal/learner",
			"WHERE created_at > NOW() - INTERVAL '7 days' AND serve_source NOT LIKE 'cache_hit%' " +
				"AND prompt_hash <> '' GROUP BY prompt_hash HAVING COUNT(*) >= 3"},
		{"IS DISTINCT FROM form", "WHERE prompt_hash IS DISTINCT FROM '' GROUP BY prompt_hash"},
		{"length form", "WHERE length(prompt_hash) > 0 GROUP BY prompt_hash"},
	}
	for _, c := range accept {
		if !excludesTheDefault(strings.Join(strings.Fields(c.sql), " "), "prompt_hash") {
			t.Errorf("exemption rejects the sanctioned self-correcting shape (%s): %q — the fix "+
				"would have to be frozen into an allowlist instead", c.name, c.sql)
		}
	}

	reject := []struct{ name, sql string }{
		{"internal/learner AS IT SHIPPED, before the fix",
			"WHERE created_at > NOW() - INTERVAL '7 days' AND serve_source NOT LIKE 'cache_hit%' " +
				"GROUP BY prompt_hash HAVING COUNT(*) >= 3"},
		{"internal/warmer candidatesSQL, which has no such test",
			"FROM prompt_embeddings pe JOIN token_events te ON te.prompt_hash = pe.prompt_hash " +
				"WHERE pe.hit_count >= 5"},
		{"an emptiness test on a DIFFERENT column must not exempt this one",
			"WHERE prompt_text <> '' GROUP BY prompt_hash"},
		{"merely naming the column is not a test of it",
			"SELECT prompt_hash FROM token_events GROUP BY prompt_hash"},
	}
	for _, c := range reject {
		flat := strings.Join(strings.Fields(c.sql), " ")
		for _, n := range []string{"prompt_hash", "te.prompt_hash"} {
			if excludesTheDefault(flat, n) {
				t.Errorf("exemption ACCEPTS a statement it must flag (%s) via %q: %q — this would "+
					"silently disable the guard", c.name, n, c.sql)
			}
		}
	}
}

// ⚠ END-TO-END CONTROL: the guard as a whole must go red on the statement that actually shipped.
// The two controls above test the pieces; this one runs the real matcher over the real text, so a
// wiring mistake between them cannot pass unnoticed.
func TestGuardGoesRedOnTheStatementThatShipped(t *testing.T) {
	shipped := strings.Join(strings.Fields(
		`SELECT prompt_hash, COUNT(*) as hit_count, AVG(input_tokens + output_tokens) as avg_tokens,
		 MAX(created_at) as last_seen FROM token_events
		 WHERE created_at > NOW() - INTERVAL '7 days' AND serve_source NOT LIKE 'cache_hit%'
		 GROUP BY prompt_hash HAVING COUNT(*) >= 3 ORDER BY hit_count DESC LIMIT 20`), " ")

	if !onlyTable(shipped) {
		t.Fatal("token_events is the only table in the shipped statement; the resolver disagrees, " +
			"so the bare-name branch this defect needs would never run")
	}
	if !qualifiedUse("prompt_hash").MatchString(shipped) {
		t.Fatal("the guard does not match the statement that shipped the defect")
	}
	if excludesTheDefault(shipped, "prompt_hash") {
		t.Fatal("the exemption swallows the statement that shipped the defect")
	}
	if _, known := matchKnown(shipped); known {
		t.Fatal("knownOffences excuses the statement that shipped the defect")
	}
}
