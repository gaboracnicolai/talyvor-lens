package config_test

// WHY THIS FILE EXISTS: THE CANONICAL BUILD-STATE MANIFEST DECAYED WITH NO COMMIT TO IT.
//
// BUILD_STATE.md calls itself the "Single source of truth for what is built, derived
// from the actual code". Its §B table is the only place an operator reads to learn
// which flags LENS_ECONOMY_ENABLED=false actually turns off. MEASURED at main
// 961c5e5, three independent decays, none of which anything in this repo could see:
//
//	(1) THE COUNT. "the force-off block force-sets **14** flags" — it force-sets 16.
//	    The two missing were AnnotationMintingEnabled and ConfidentialMintingEnabled,
//	    both MINT gates, and NEITHER APPEARED ANYWHERE IN THE DOCUMENT.
//	(2) THE CITATIONS. §B cited config.go BY LINE. 30 of 30 were wrong — median 221
//	    lines off, NONE within ±2. Worse than stale: LENS_POVI_MINTING_ENABLED pointed
//	    at `ProofOfImprovementEnabled bool` and LENS_REPUTATION_BONDED_MINTING_ENABLED
//	    at `RoutingPredictionEnabled bool`, so a reader following a money-flag citation
//	    landed on a DIFFERENT money flag and read its rules.
//	(3) THE MECHANISM. The header promised "Regenerated (never hand-edited) whenever it
//	    goes stale" and no generator exists anywhere in the repository.
//
// ⚠ NOBODY WAS CARELESS, AND THAT IS THE POINT. At 7f2ebd2 — the SHA the file declares
// — the force-off block held EXACTLY those 14 and every line number was right. #269
// added the confidential mint and #354 added the annotation mint; config.go grew ~500
// lines. Every claim became false with NO edit to BUILD_STATE.md. No review catches
// that and no test could fail for it, because the premise lived in another file.
//
// THE FIX IS NOT BETTER NUMBERS — a recomputed line rots identically the next time
// config.go moves. It is the SYMBOL, which survives an edit and cannot come to point
// at a different declaration. Rule D below refuses a line citation outright so the
// class cannot come back.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	manifestPath   = "../../BUILD_STATE.md"
	configFilePath = "config.go"
)

// FLOORS. Every rule here is a set comparison, and a set comparison over an EMPTY set
// passes. These stand between "the manifest agrees with config.go" and "my parser read
// nothing and reported a clean document". Measured at the commit that added this file:
// 38 §B rows, 16 force-off assignments.
const (
	floorManifestRows = 30
	floorForceOff     = 16
)

var manifestRowRE = regexp.MustCompile(`^\|\s*` + "`" + `(LENS_[A-Z0-9_]+)` + "`" + `\s*\|([^|]*)\|([^|]*)\|([^|]*)\|`)

type manifestRow struct {
	line     int
	env      string
	citation string // the "config.go field" cell, verbatim
	forceOff string // the "In force-off block?" cell, verbatim
}

func readManifestRows(t *testing.T) []manifestRow {
	t.Helper()
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", manifestPath, err)
	}
	var out []manifestRow
	for i, l := range strings.Split(string(raw), "\n") {
		m := manifestRowRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		out = append(out, manifestRow{
			line:     i + 1,
			env:      m[1],
			citation: strings.TrimSpace(m[3]),
			forceOff: strings.TrimSpace(m[4]),
		})
	}
	return out
}

func manifestText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", manifestPath, err)
	}
	return string(raw)
}

// configForceOffFlags parses the flags config.go sets false inside
// `if !c.EconomyEnabled { ... }` — read from the code that performs the force-off,
// never from a comment describing it.
func configForceOffFlags(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, configFilePath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", configFilePath, err)
	}
	found := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		un, ok := ifs.Cond.(*ast.UnaryExpr)
		if !ok || un.Op != token.NOT {
			return true
		}
		sel, ok := un.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "EconomyEnabled" {
			return true
		}
		for _, stmt := range ifs.Body.List {
			as, ok := stmt.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				continue
			}
			lhs, lok := as.Lhs[0].(*ast.SelectorExpr)
			rhs, rok := as.Rhs[0].(*ast.Ident)
			if lok && rok && rhs.Name == "false" {
				found[lhs.Sel.Name] = true
			}
		}
		return true
	})
	return found
}

// configFields returns every field name declared on Config.
func configFields(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, configFilePath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", configFilePath, err)
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Config" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, fld := range st.Fields.List {
			for _, name := range fld.Names {
				out[name.Name] = true
			}
		}
		return false
	})
	return out
}

func configSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(configFilePath)
	if err != nil {
		t.Fatalf("read %s: %v", configFilePath, err)
	}
	return string(raw)
}

// RULE A — every §B citation must name a REAL Config field.
//
// The rule that would have caught 30 wrong line numbers, restated so it cannot be
// satisfied by a number that merely looks plausible.
func TestManifestCitationsNameRealConfigFields(t *testing.T) {
	rows := readManifestRows(t)
	if len(rows) < floorManifestRows {
		t.Fatalf("parsed only %d rows from %s's §B table (floor %d) — the table markup moved or "+
			"the row regex matched nothing, and every check below would pass vacuously",
			len(rows), manifestPath, floorManifestRows)
	}
	fields := configFields(t)
	if len(fields) < floorManifestRows {
		t.Fatalf("parsed only %d Config fields (floor %d) — the struct walk read nothing",
			len(fields), floorManifestRows)
	}

	for _, r := range rows {
		sym := strings.Trim(r.citation, "` ")
		if sym == "" {
			t.Errorf("%s:%d %s cites nothing", manifestPath, r.line, r.env)
			continue
		}
		if !fields[sym] {
			t.Errorf("%s:%d %s cites config field %q and config.Config has no such field — a "+
				"citation a reader cannot follow is worse than none, because it looks checkable",
				manifestPath, r.line, r.env, sym)
		}
	}
}

// RULE B — the cited field must be the one that env var actually sets.
//
// A citation naming a REAL field that belongs to a DIFFERENT flag is the exact failure
// the line numbers produced: LENS_POVI_MINTING_ENABLED pointed at a real declaration,
// just not its own. Rule A alone would have passed on that.
func TestManifestCitationsPointAtTheirOwnFlag(t *testing.T) {
	rows := readManifestRows(t)
	if len(rows) < floorManifestRows {
		t.Fatalf("parsed only %d rows (floor %d) — vacuous comparison refused", len(rows), floorManifestRows)
	}
	src := configSource(t)

	for _, r := range rows {
		sym := strings.Trim(r.citation, "` ")
		if sym == "" {
			continue
		}
		// The env var and the field must appear on, or adjacent to, the same statement:
		// config.go always reads an env var into its field in one place.
		if !envReadsIntoField(src, r.env, sym) {
			t.Errorf("%s:%d %s cites %q, but config.go never reads that variable into that "+
				"field — the citation points at somebody else's flag, which is how a reader "+
				"ends up applying the wrong rules to a money switch",
				manifestPath, r.line, r.env, sym)
		}
	}
}

// envReadsIntoField reports whether config.go reads env into field. It accepts every
// shape config.go uses: a struct-literal parseBoolEnv/parseXEnv, a bare assignment, the
// default-on {&c.Field, "ENV"} loop, and an os.Getenv guard followed by an assignment
// within the same short block.
func envReadsIntoField(src, env, field string) bool {
	quoted := regexp.QuoteMeta(`"` + env + `"`)
	direct := []string{
		regexp.QuoteMeta(field) + `:\s*parse\w+Env\(` + quoted,
		`c\.` + regexp.QuoteMeta(field) + `\s*=\s*parse\w+Env\(` + quoted,
		`\{&c\.` + regexp.QuoteMeta(field) + `,\s*` + quoted + `\}`,
	}
	for _, p := range direct {
		if regexp.MustCompile(p).MatchString(src) {
			return true
		}
	}
	// os.Getenv("ENV") guard: the assignment to the field must follow within the block.
	loc := regexp.MustCompile(`os\.Getenv\(` + quoted + `\)`).FindStringIndex(src)
	if loc == nil {
		return false
	}
	tail := src[loc[1]:]
	if len(tail) > 4000 {
		tail = tail[:4000]
	}
	return regexp.MustCompile(`c\.` + regexp.QuoteMeta(field) + `\s*=`).MatchString(tail)
}

// RULE C — the "In force-off block?" column must match config.go, both directions, and
// every flag config.go force-offs must HAVE a row.
//
// This is the rule that reds on the two mint gates that were absent from the whole
// document: an operator reading §B to learn what the master switch turns off was shown
// a table that did not mention them.
func TestManifestForceOffColumnMatchesConfig(t *testing.T) {
	rows := readManifestRows(t)
	if len(rows) < floorManifestRows {
		t.Fatalf("parsed only %d rows (floor %d) — vacuous comparison refused", len(rows), floorManifestRows)
	}
	inBlock := configForceOffFlags(t)
	if len(inBlock) < floorForceOff {
		t.Fatalf("parsed only %d flags from config.go's force-off block (floor %d) — the parser "+
			"read nothing or read the wrong block", len(inBlock), floorForceOff)
	}
	src := configSource(t)

	documented := map[string]bool{}
	for _, r := range rows {
		sym := strings.Trim(r.citation, "` ")
		if sym == "" || !envReadsIntoField(src, r.env, sym) {
			continue // rule B already reds on this row; do not double-report
		}
		documented[sym] = true

		saysYes := strings.Contains(strings.ToLower(r.forceOff), "yes")
		if inBlock[sym] && !saysYes {
			t.Errorf("%s:%d %s (%s) IS in config.go's force-off block and the table says %q — a "+
				"reader is told the master switch leaves this flag alone when it does not",
				manifestPath, r.line, r.env, sym, r.forceOff)
		}
		if !inBlock[sym] && saysYes {
			t.Errorf("%s:%d %s (%s) is NOT in config.go's force-off block and the table says %q — "+
				"a reader is told the master switch disarms this flag when it does not",
				manifestPath, r.line, r.env, sym, r.forceOff)
		}
	}

	var missing []string
	for f := range inBlock {
		if !documented[f] {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	for _, f := range missing {
		t.Errorf("config.go force-offs %q and §B has no row for it — this table is the only place "+
			"an operator reads to learn what LENS_ECONOMY_ENABLED=false turns off, and a flag "+
			"absent from it is indistinguishable from a flag that does not exist", f)
	}
}

// RULE D — no §B row may cite config.go by LINE, ever again.
//
// The class fix, not the instance fix. A recomputed line number is wrong the next time
// config.go moves, and the whole point of the symbol citation is that it cannot come to
// point at another declaration. This refuses the shape rather than the current values.
func TestManifestHasNoLineCitations(t *testing.T) {
	rows := readManifestRows(t)
	if len(rows) < floorManifestRows {
		t.Fatalf("parsed only %d rows (floor %d) — vacuous comparison refused", len(rows), floorManifestRows)
	}
	lineCite := regexp.MustCompile(`:\d+`)
	for _, r := range rows {
		if lineCite.MatchString(r.citation) {
			t.Errorf("%s:%d %s cites config.go by LINE (%q). Line citations decay with no commit "+
				"to this file — 30 of 30 were wrong when this rule was written, median 221 lines "+
				"off. Cite the Go field SYMBOL instead: it survives an edit and cannot come to "+
				"point at a different flag", manifestPath, r.line, r.env, r.citation)
		}
	}
}

// RULE E — the kill-switch paragraph's COUNT and its explicit name list must both match
// config.go.
//
// The count and the list agreed with each other the entire time they were both wrong,
// which is why nothing in the document disclosed the omission. They are checked against
// the code, separately, because two restatements agreeing proves only that they were
// copied from one another.
func TestKillSwitchCountAndListMatchConfig(t *testing.T) {
	inBlock := configForceOffFlags(t)
	if len(inBlock) < floorForceOff {
		t.Fatalf("parsed only %d force-off flags (floor %d) — vacuous comparison refused",
			len(inBlock), floorForceOff)
	}
	text := manifestText(t)

	countRE := regexp.MustCompile(`force-off block force-sets \*\*(\d+)\*\* flags false`)
	m := countRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("could not find the kill-switch count sentence in %s — if it was reworded, this "+
			"rule stopped reading anything and must be re-anchored, not deleted", manifestPath)
	}
	stated, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("kill-switch count %q is not a number", m[1])
	}
	if stated != len(inBlock) {
		t.Errorf("the kill-switch paragraph says the force-off block force-sets %d flags; "+
			"config.go force-sets %d. This is the sentence an operator reads FIRST",
			stated, len(inBlock))
	}

	// The explicit name list beside it must be exactly the block, both directions.
	listed := map[string]bool{}
	for _, f := range regexp.MustCompile(`\b([A-Z]\w*Enabled)\b`).FindAllStringSubmatch(killSwitchList(t, text), -1) {
		listed[f[1]] = true
	}
	if len(listed) < floorForceOff {
		t.Fatalf("only %d flag names parsed out of the kill-switch list (floor %d) — the list "+
			"markup moved and this rule would pass over any omission", len(listed), floorForceOff)
	}
	for f := range inBlock {
		if !listed[f] {
			t.Errorf("config.go force-offs %q and the kill-switch list does not name it", f)
		}
	}
	for f := range listed {
		if !inBlock[f] {
			t.Errorf("the kill-switch list names %q and config.go does NOT force it off — the "+
				"paragraph promises a disarm that does not happen", f)
		}
	}
}

// killSwitchList returns the backticked flag-name list that follows the count sentence.
func killSwitchList(t *testing.T, text string) string {
	t.Helper()
	idx := strings.Index(text, "force-off block force-sets")
	if idx < 0 {
		t.Fatal("kill-switch paragraph not found")
	}
	rest := text[idx:]
	start := strings.Index(rest, "`")
	if start < 0 {
		t.Fatal("kill-switch flag list not found")
	}
	rest = rest[start+1:]
	end := strings.Index(rest, "`")
	if end < 0 {
		t.Fatalf("kill-switch flag list is not terminated in %s", manifestPath)
	}
	return rest[:end]
}
