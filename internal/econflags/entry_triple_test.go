package econflags

// WHY THIS FILE EXISTS: THE READOUT'S TABLE HAS THREE COLUMNS AND ONLY ONE WAS CHECKED.
//
// Every entry in Report's table is a three-column transcription of config.go:
//
//	{"BillingEnabled", "LENS_BILLING_ENABLED", cfg.BillingEnabled}
//	  ^ the NAME         ^ the VARIABLE          ^ the VALUE READ
//
// Rules A, B, C, D and E all key on the NAME. Not one of them looks at the other two, so
// a name that is correct carries a variable and a value that are unchecked — and this is a
// transcription, so it decays exactly the way econflags.forceOff decayed before rule A.
//
// MEASURED, NOT ARGUED (harness ~/talyvor-queue/w61-entries-triple-controls-7a2e.py, every
// mutation restored in a finally and sha256-verified, run over internal/econflags AND
// cmd/lens — the only two packages that touch this readout):
//
//	VALUE COLUMN — point each entry's cfg.X at a DIFFERENT field: 22 of 26 NOT CAUGHT.
//	The blind set includes AdminLXCGrantEnabled, which this package's own doc comment
//	calls "the one flag in the readout that lets credit come into existence without
//	revenue", and LXCReservationEnabled, which decides whether the customer is billed the
//	delivered cost or a pre-serve estimate. The four that ARE caught are caught by tests
//	that happen to assert a value (PatternEarning, POVIMinting, Billing, Batch), not by
//	any rule about the table.
//
//	ENV COLUMN — point each entry's env at ANOTHER FLAG'S REAL VARIABLE: 26 of 27 NOT
//	CAUGHT. TestReportedEnvNamesMatchConfig's docstring claimed exactly this catch — "a
//	copy-paste that names the wrong variable reds here rather than sending an operator to
//	edit a variable the process never looks at" — and it could not make it, because it
//	compared the reported env against the SET of every parseBoolEnv literal in config.go.
//	A wrong-but-real variable is a member of that set. ⚠ AND THE MISS IS IN THE WORSE
//	DIRECTION: the variable is one the process DOES look at, so an operator following the
//	readout does not change nothing, they change a DIFFERENT money flag.
//
// THAT OLD TEST IS DELETED AND ITS FILE SAYS SO. Rule G below is a strict superset: an env
// var no helper reads anywhere fails a per-field check too (control C4 proves it), and rule
// G additionally catches the 26 the set check could not see.
//
// ⚠ IT ALSO CARRIED A FAILURE MESSAGE THAT WAS FALSE BY CONSTRUCTION FOR A WHOLE CLASS OF
// FLAGS. It scanned parseBoolEnv call sites ONLY, while config.go reads four flags through
// parseBoolEnvDefaultTrue (RoutingDecisionCaptureEnabled, KeelRoyaltyHaircutEnabled,
// ModelWatchEnabled, KeelHardenedEnabled). Adding any of them to the readout — the exact
// widening this package's own comments ask for — redded that test with the message
// "config.go never reads that variable — an operator setting it would change nothing".
// MEASURED on unfixed main in a scratch worktree, not inferred: that is the verbatim output
// for LENS_KEEL_ROYALTY_HAIRCUT_ENABLED, which config.go reads at the parseBoolEnvDefaultTrue
// call site beside KeelRoyaltyHaircutEnabled. The rule was green only because none of the
// four is reported yet, and its message would have sent whoever widened it looking for a
// defect in config.go that is not there. The helper set below is DERIVED from config.go
// instead of named, so a third helper does not re-arm the trap — control C7 hardcodes it
// back and the false message returns.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

const econflagsPath = "econflags.go"

// FLOORS. Both rules below are per-item comparisons over a parsed set, and a comparison
// over an empty set passes. These are what stands between "the table agrees with config.go"
// and "my parser read nothing and reported a clean product". Measured at the commit that
// introduced this file, by raising each floor above reality and reading what it reported:
// 27 entries in the table, 60 Config fields read from a bool env var.
const (
	floorEntryTriples  = 25
	floorEnvReadFields = 50
)

// reportEntry is one row of Report's table as the SOURCE writes it, not as Report returns
// it — Report() hands back a Flag whose Value is already a bool, so the field it was read
// from is gone by then. The value column is only observable in the AST.
type reportEntry struct {
	name       string // column 1: the flag's name
	env        string // column 2: the variable an operator is told to set
	valueField string // column 3: the Config field actually read
	pos        string
}

// boolEnvHelpers derives, from config.go itself, the set of functions that turn an
// environment variable into a bool: one string parameter, one bool result, and a body that
// calls os.Getenv/os.LookupEnv ON THAT PARAMETER.
//
// ⚠ DERIVED RATHER THAN NAMED ON PURPOSE. A hardcoded {"parseBoolEnv"} is what made the
// deleted test's message false: config.go grew parseBoolEnvDefaultTrue and the scan did
// not. A named set is a claim about today's config.go maintained by nobody; this is a
// property of the declaration, so a third helper is picked up with no edit here.
func boolEnvHelpers(t *testing.T, f *ast.File) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Body == nil {
			continue
		}
		if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) != 1 {
			continue
		}
		pt, ok := fn.Type.Params.List[0].Type.(*ast.Ident)
		if !ok || pt.Name != "string" {
			continue
		}
		param := fn.Type.Params.List[0].Names[0].Name
		if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
			continue
		}
		rt, ok := fn.Type.Results.List[0].Type.(*ast.Ident)
		if !ok || rt.Name != "bool" {
			continue
		}
		readsParam := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "os" {
				return true
			}
			if sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv" {
				return true
			}
			if arg, ok := call.Args[0].(*ast.Ident); ok && arg.Name == param {
				readsParam = true
			}
			return true
		})
		if readsParam {
			out[fn.Name.Name] = true
		}
	}
	return out
}

// envReadsByField parses config.go and returns, per Config field, every environment
// variable config.go reads a bool from INTO THAT FIELD. Both shapes config.go uses are
// covered: the keyed struct literal (`BillingEnabled: parseBoolEnv("...")`) and the plain
// assignment (`c.KeelHardenedEnabled = parseBoolEnvDefaultTrue("...")`).
func envReadsByField(t *testing.T) (byField map[string][]string, byEnv map[string][]string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, configPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", configPath, err)
	}
	helpers := boolEnvHelpers(t, f)
	if len(helpers) == 0 {
		t.Fatalf("no bool-from-env helper found in %s — the derivation matched nothing and every "+
			"per-field comparison below would be made against an empty map", configPath)
	}

	byField, byEnv = map[string][]string{}, map[string][]string{}
	record := func(field, env string) {
		for _, e := range byField[field] {
			if e == env {
				return
			}
		}
		byField[field] = append(byField[field], env)
		byEnv[env] = append(byEnv[env], field)
	}
	envArg := func(e ast.Expr) (string, bool) {
		call, ok := e.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return "", false
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || !helpers[id.Name] {
			return "", false
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		return strings.Trim(lit.Value, `"`), true
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.KeyValueExpr:
			key, ok := node.Key.(*ast.Ident)
			if !ok {
				return true
			}
			if env, ok := envArg(node.Value); ok {
				record(key.Name, env)
			}
		case *ast.AssignStmt:
			if len(node.Lhs) != 1 || len(node.Rhs) != 1 {
				return true
			}
			sel, ok := node.Lhs[0].(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if env, ok := envArg(node.Rhs[0]); ok {
				record(sel.Sel.Name, env)
			}
		}
		return true
	})
	return byField, byEnv
}

// reportEntries parses Report's own table out of econflags.go. Reading the SOURCE rather
// than calling Report() is the whole point: two of the three columns are erased by the time
// Report returns.
func reportEntries(t *testing.T) []reportEntry {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, econflagsPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", econflagsPath, err)
	}

	var out []reportEntry
	var malformed []string
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		at, ok := cl.Type.(*ast.ArrayType)
		if !ok {
			return true
		}
		elt, ok := at.Elt.(*ast.Ident)
		if !ok || elt.Name != "entry" {
			return true
		}
		for _, e := range cl.Elts {
			row, ok := e.(*ast.CompositeLit)
			pos := fset.Position(e.Pos()).String()
			if !ok || len(row.Elts) != 3 {
				malformed = append(malformed, pos)
				continue
			}
			name, ok1 := row.Elts[0].(*ast.BasicLit)
			env, ok2 := row.Elts[1].(*ast.BasicLit)
			val, ok3 := row.Elts[2].(*ast.SelectorExpr)
			if !ok1 || !ok2 || !ok3 || name.Kind != token.STRING || env.Kind != token.STRING {
				malformed = append(malformed, pos)
				continue
			}
			out = append(out, reportEntry{
				name:       strings.Trim(name.Value, `"`),
				env:        strings.Trim(env.Value, `"`),
				valueField: val.Sel.Name,
				pos:        pos,
			})
		}
		return true
	})
	// A row this walk cannot read is a row neither rule below can check, and skipping it
	// silently is how a scan reports a clean product over something it never looked at.
	if len(malformed) > 0 {
		t.Fatalf("%d entr(ies) in %s are not the {name, env, cfg.Field} shape these rules read "+
			"(%s) — they would be silently unchecked", len(malformed), econflagsPath,
			strings.Join(malformed, ", "))
	}
	return out
}

func requireFloors(t *testing.T, entries []reportEntry, byField map[string][]string) {
	t.Helper()
	if len(entries) < floorEntryTriples {
		t.Fatalf("parsed only %d entries out of %s's table (floor %d) — the walk read nothing or "+
			"read the wrong literal, and every per-entry comparison below would pass vacuously",
			len(entries), econflagsPath, floorEntryTriples)
	}
	if len(byField) < floorEnvReadFields {
		t.Fatalf("parsed only %d Config fields read from a bool env var in %s (floor %d) — the "+
			"assignment walk read nothing, and rule G would pass over any wrong variable",
			len(byField), configPath, floorEnvReadFields)
	}
}

// RULE F — THE VALUE COLUMN. Every entry must read the field it names.
//
// `{"BillingEnabled", "LENS_BILLING_ENABLED", cfg.LXCGatingEnabled}` compiles, satisfies
// rules A/B/C/D/E — all five check the string — and reports the fiat door's state from an
// unrelated flag. Measured: 22 of 26 entries can be given another field's value with
// internal/econflags and cmd/lens fully green.
func TestEveryEntryReadsTheFieldItNames(t *testing.T) {
	entries := reportEntries(t)
	byField, _ := envReadsByField(t)
	requireFloors(t, entries, byField)

	for _, e := range entries {
		if e.valueField != e.name {
			t.Errorf("%s: flag %q reports the value of Config.%s — the readout would state %s's "+
				"condition from a different flag, and every existing rule here checks the NAME "+
				"only, so all of them stay green", e.pos, e.name, e.valueField, e.name)
		}
	}
}

// RULE G — THE ENV COLUMN. Every entry must name the variable config.go reads INTO THAT
// FIELD, not merely a variable config.go reads somewhere.
//
// This is the rule the deleted TestReportedEnvNamesMatchConfig claimed and could not keep.
// Its check was set membership over every parseBoolEnv literal, so 26 of 27 entries could
// name another flag's real variable and stay green.
func TestEveryEntryNamesTheVariableThatSetsIt(t *testing.T) {
	entries := reportEntries(t)
	byField, byEnv := envReadsByField(t)
	requireFloors(t, entries, byField)

	for _, e := range entries {
		if e.env == "" {
			t.Errorf("%s: flag %q reports no env var, so the readout cannot be acted on", e.pos, e.name)
			continue
		}
		reads := byField[e.name]
		if contains(reads, e.env) {
			continue
		}
		// The message is built from the parsed map, so it states what config.go DOES rather
		// than asserting what it does not. The deleted rule said "config.go never reads that
		// variable" — false whenever the variable is read by a helper the scan did not know.
		switch {
		case len(reads) == 0:
			t.Errorf("%s: flag %q reports env %q, and config.go reads NO bool env var into "+
				"Config.%s at all%s", e.pos, e.name, e.env, e.name, whoElseReads(byEnv, e.env))
		default:
			t.Errorf("%s: flag %q reports env %q, but config.go reads %s into Config.%s%s — an "+
				"operator following this readout would set a variable that changes something "+
				"else", e.pos, e.name, e.env, quoteAll(reads), e.name, whoElseReads(byEnv, e.env))
		}
	}
}

func whoElseReads(byEnv map[string][]string, env string) string {
	fields := byEnv[env]
	if len(fields) == 0 {
		return "; no helper in config.go reads that variable either"
	}
	sorted := append([]string(nil), fields...)
	sort.Strings(sorted)
	return fmt.Sprintf("; %q is read into Config.%s", env, strings.Join(sorted, ", Config."))
}

func quoteAll(ss []string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(out, " and ")
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
