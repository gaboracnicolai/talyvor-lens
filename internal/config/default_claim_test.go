package config

// WHY THIS FILE EXISTS: THREE COMMENTS SAID "DEFAULT-OFF" ABOUT TWO FLAGS THIS REPO'S OWN
// TESTS PIN DEFAULT-ON, AND TWO OF THEM SAT ON THE LINE THAT INSTALLS A MONEY SEAM.
//
// MEASURED, NOT READ:
//
//	config.go            KeelHardenedEnabled     bool // LENS_KEEL_HARDENED_ENABLED (default FALSE)
//	cmd/lens/main.go, at the K3 hardened-emission wiring:
//	                     "K3 money-grade hardened emission — DEFAULT-OFF (cfg.KeelHardenedEnabled)"
//	cmd/lens/main.go, at the KE-2 SetDriftHaircut wiring:
//	                     "KE-2 ... DEFAULT-OFF (it changes money — off until N3 calibration).
//	                     When off the seam is never wired, so the mint path is byte-identical."
//
// ⚠ CITED BY SYMBOL, NOT BY LINE: the pointer audit reds a new file carrying line citations,
// and it is right to — a comment edit two files over is exactly what moves them.
//
// Both flags are loaded with parseBoolEnvDefaultTrue, and keel_defaults_test.go asserts both
// default TRUE with the reason written out ("this run's flip"). So the defaults are deliberate
// and the comments are stale — and the KE-2 one is the expensive kind, because its second
// sentence tells the reader a CONSEQUENCE: that the reduce-only haircut on reuse royalties is
// not installed. On a default deployment it is installed, and it can only LOWER a contributor's
// mint. A reader auditing what touches a mint would have crossed that line off the list.
//
// A "default" written in a comment is a claim about the loader a thousand lines away, and
// nothing could compare the two. This rule does, by OBSERVING the loader (clear every LENS_*
// variable, call Load(), read the struct) rather than reading its shape — see observedDefaults
// for why the shape is not enough.
//
// ⚠ AND IT FOUND FOUR MORE, WHICH IS WHY IT IS A RULE AND NOT THREE EDITS. Measured at the
// commit that introduced this file, over 43 checkable claims on 60 bool fields, the full red
// set was: CachePoolableEnabled ("Default false" ⇒ true), PatternEarningEnabled ("DEFAULT
// FALSE" ⇒ true), SettlementFailClosedEnabled ("DEFAULT FALSE" twice ⇒ true — a fail-closed
// settlement posture documented as unarmed while it ships armed), KeelEnabled, and the two
// above. Every one of those defaults is deliberate and pinned by a test elsewhere
// (keel_defaults_test.go, ke2_haircut_flag_test.go); it is the comments that were stale, so
// this commit corrects comments and changes NO default.
//
// ⚠ THE SIXTH RED WAS A DOC-COMMENT THAT HAD SLID ONTO THE WRONG FIELD. BatchEnabled was
// accused of claiming "default TRUE" when it is false. It does not: EconomyEnabled's doc block
// — the MASTER economy kill-switch, "Env: LENS_ECONOMY_ENABLED, default TRUE" — was glued to
// BatchEnabled's with no blank line, so the parser (and godoc, and any reader) attributed the
// kill-switch's documentation to the batch lane. The blank line is restored in this commit and
// the accusation goes away with it. A rule that reads comments the way the parser does is what
// made that visible.
//
// ⚠ SCOPE, AND IT IS NARROWER THAN THE CLASS: this rule reads config.go ONLY. The two
// cmd/lens comments above are corrected by hand in the same commit and are NOT guarded —
// their premise is in this package, so a rule over that file is possible, but it is a
// different file walk resting on a different measurement and one merge should not smuggle it
// in. What is guarded here is the file where the loader and the claim live together.
//
// ─────────────────────────────────────────────────────────────────────────────────────────
// ⚠⚠ WIDENED, AND THE RULE ABOVE WAS GREEN OVER FIVE FALSE CLAIMS WHEN IT WAS.
//
// Everything above this line was true when it was written and TWO PARTS OF IT ARE NOW STALE,
// which is why they are struck through here rather than edited away: the "SCOPE" paragraph
// no longer holds (TestDefaultClaimsAtTheWiringSitesMatchTheLoader below reads every other
// non-test .go file in the tree), and "the full red set was" understated itself by two.
//
// THE INSTRUMENT HAD TWO BLINDNESSES AND EACH HID A DIFFERENT PAIR OF LIVE FALSE CLAIMS:
//
//  1. THE LINE WRAP. commentText joined c.Text — which still carries the leading `//` — so a
//     claim written across a line break was scanned as `Default // false`, and the separator
//     class `[-: ]*` has no slash in it. Not merely unmatched: UNMATCHABLE. It hid
//     PoolRoyaltyMintingEnabled ("DEFAULT\n// FALSE — with this off ... NOTHING mints") and
//     DistillPoolableEnabled ("Default\n// false ... Inert by default"). ⚠ BOTH SIT IN THE
//     SAME DEFAULT-ON LOOP IN Load AS CachePoolableEnabled, WHICH THE COMMIT ABOVE CORRECTED
//     BY HAND THREE LINES AWAY. That loop has four entries; that commit fixed two of them and
//     its guard could not see the other two. A hand audit does not close this class, and the
//     guard written to close it was green over half the loop it was written for.
//     ⚠ THE EXPENSIVE ONE IS THE ROYALTY MINT: "NOTHING mints" is a claim about a DEFAULT
//     DEPLOYMENT, and measured, every switch in that chain is armed — the flag, the pooling
//     gate CachePoolableEnabled, and migration 0106's workspaces.cache_poolable DEFAULT true.
//
//  2. THE FILE SCOPE, which the paragraph above declared and did not close. Three more, all
//     in cmd/lens/main.go, all stating the CONSEQUENCE backwards as well as the default —
//     see TestDefaultClaimsAtTheWiringSitesMatchTheLoader's own doc for the list. One of
//     them, SettlementFailClosedEnabled, was contradicted by ANOTHER comment about the SAME
//     flag in the SAME file — the pool-royalty finalize sweeper said "Default OFF", the
//     traffic-mint sweeper below it said "DEFAULT-ON". One file, one flag, two answers.
//
// This commit corrects five comments and changes NO default; the product diff is comment-only.
//
// ─────────────────────────────────────────────────────────────────────────────────────────
// ⚠⚠ WIDENED AGAIN, AND RULE H WAS GREEN OVER TWO MORE FALSE CLAIMS ABOUT THE SAME FLAG IT
// HAD JUST BEEN WRITTEN TO CATCH.
//
// THE THIRD BLINDNESS IS THE BINDING'S WIDTH: documentedLine reads EXACTLY ONE LINE. A
// wire-up written as a multi-line call puts the flag beyond it. Measured: the settlement
// CLEARERS are constructed over three lines and their only gate — `func() bool { return
// cfg.SettlementFailClosedEnabled }` — is on the THIRD, so documentedLine returned
// `poolClearer := poolroyalty.NewSettlementClearer(`, which names no flag at all.
//
//	cmd/lens/main.go, at the poolClearer/distillClearer wire-up:
//	                  "DEFAULT OFF; fail-closed on a detector error"        ⇒ the flag is TRUE
//	cmd/lens/main.go, at the routingClearer/evalClearer wire-up:
//	                  "Same default-off/fail-closed/scan-window discipline" ⇒ the flag is TRUE
//
// ⚠ CITED BY SYMBOL, NOT BY LINE, for the reason this file's own header already gives — and
// I proved it on myself: my first cut of this block cited both by line, and correcting the
// FIRST comment shifted the SECOND by two, so one of the two pointers was stale inside the
// same commit that wrote it. The pointer audit reds a new citing file, and it was right to.
//
// ⚠ THIS IS THE SAME FLAG, IN THE SAME FILE, AS THE "one file, one flag, two answers" DEFECT
// THE PARAGRAPH ABOVE NAMED — and the count was not two. After that commit corrected the two
// finalize sweepers (the pool-royalty and traffic-mint SetSettleStatus("cleared") calls) to
// DEFAULT-ON, cmd/lens/main.go said DEFAULT-ON about SettlementFailClosedEnabled twice and
// DEFAULT OFF about it twice, and the instrument written for exactly that contradiction
// could reach only the half it had already fixed.
//
// ⚠ WHAT THE FALSE HALF CLAIMS, AND WHY IT IS THE EXPENSIVE DIRECTION: the SettlementClearer
// is, in its own file's words, "the ONLY thing that promotes held→cleared", and the
// fail-closed FinalizeSweeper settles ONLY 'cleared' rows. So these two comments told a
// reader auditing what can move money on a default deployment that the promotion step is
// inert. Measured, every switch above them is armed: the clearers sit inside `if
// cfg.EconomyEnabled` (default TRUE) and their gate is SettlementFailClosedEnabled (default
// TRUE, and pinned as a default-ARMED money flag by econflags/default_armed_census_test.go).
//
// ⚠ THE FIX IS A FALLBACK, NOT A REPLACEMENT, AND THAT IS MEASURED — see documentedStmt.
// This commit corrects two comments and changes NO default; the product diff is comment-only.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

const configSource = "config.go"

// repoRoot is where the wiring-site walk starts. config.go is this package's own file;
// the flags it declares are READ a thousand lines away in another package, and that is
// where the claims this rule was widened for live.
const repoRoot = "../.."

// FLOORS. Both halves below are comparisons over a parsed set, and a comparison over an empty
// set passes. Measured at the commit that introduced this file by raising each floor above
// reality and reading what it reported: 60 bool fields on the loaded Config, 43 of them
// carrying a default claim in a comment.
const (
	floorDerivedDefaults = 45
	floorDefaultClaims   = 15
)

// FLOORS FOR THE WIRING-SITE RULE. Same argument, different walk: it reads every non-test
// .go file in the tree, and a walk that parsed none of them, or a binding that resolved
// none of them, reports a clean product. Measured at the commit that widened this file:
// 377 files scanned, 20 claims bound to exactly one cfg.<Field>, 2 skipped as ambiguous.
// Set below today's numbers so an ordinary edit does not red them, far enough above zero
// that a walk reading nothing does.
const (
	floorWiringFilesScanned = 200
	floorWiringClaims       = 12
)

// defaultClaim matches the ways this file's comments state a default. Deliberately broad on
// the separator and the word (DEFAULT-OFF, default false, defaults to true) and deliberately
// narrow on what follows: only the four words that ARE a boolean default.
var defaultClaim = regexp.MustCompile(`(?i)default(?:s)?[-: ]*(?:to )?(on|off|true|false)\b`)

func claimIsTrue(word string) bool {
	switch strings.ToLower(word) {
	case "on", "true":
		return true
	}
	return false
}

// observedDefaults returns, per Config bool field, the value the loader ACTUALLY produces
// when the environment is unset — by clearing every LENS_* variable, calling Load(), and
// reading the struct. It is an observation, not an inference.
//
// ⚠ MY FIRST CUT INFERRED IT FROM THE HELPER (parseBoolEnv ⇒ false) AND WAS WRONG ABOUT FOUR
// FIELDS. config.go also uses `c.X = true` followed by `if os.Getenv("E") != "" { c.X =
// parseBoolEnv("E") }` — the helper says false and the default is TRUE. The inferring version
// accused DetectorSweepEnabled, LXCAgentAllocationEnabled, LXCReservationEnabled and
// BatchEnabled of stale comments that are correct. A rule that reads the loader's SHAPE is
// another transcription; this reads the loader's ANSWER, which is the same discipline the
// econflags readout is built on ("a default is not an observation" — applied to the
// instrument rather than to the product).
func observedDefaults(t *testing.T) map[string]bool {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "LENS_") {
			old := os.Getenv(k)
			os.Unsetenv(k)
			kk, vv := k, old
			t.Cleanup(func() { os.Setenv(kk, vv) })
		}
	}
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with a cleared environment: %v", err)
	}

	out := map[string]bool{}
	v := reflect.ValueOf(*cfg)
	rt := v.Type()
	for i := 0; i < rt.NumField(); i++ {
		if rt.Field(i).Type.Kind() == reflect.Bool {
			out[rt.Field(i).Name] = v.Field(i).Bool()
		}
	}
	return out
}

// TestDefaultClaimsInCommentsMatchTheLoader — every "default off/on/true/false" written beside a
// Config field must be the default the loader actually produces.
//
// Attribution is the parser's, not a heuristic: a doc/line comment belongs to the field the Go
// parser attaches it to. That is deliberate — it is exactly how a reader with godoc open sees
// it, and it is what surfaced EconomyEnabled's block having slid onto BatchEnabled.
func TestDefaultClaimsInCommentsMatchTheLoader(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, configSource, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", configSource, err)
	}
	defaults := observedDefaults(t)
	if len(defaults) < floorDerivedDefaults {
		t.Fatalf("observed only %d bool fields on the loaded Config (floor %d) — the struct walk "+
			"read nothing and every comparison below would pass vacuously",
			len(defaults), floorDerivedDefaults)
	}

	checked := 0
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
			id, ok := fld.Type.(*ast.Ident)
			if !ok || id.Name != "bool" || len(fld.Names) != 1 {
				continue
			}
			name := fld.Names[0].Name
			want, derivable := defaults[name]
			if !derivable {
				continue
			}
			text := commentText(fld.Doc) + " " + commentText(fld.Comment)
			claims := defaultClaim.FindAllStringSubmatch(text, -1)
			if len(claims) == 0 {
				continue
			}
			// ⚠ EVERY claim is checked, not just a lone one. My first cut skipped fields
			// carrying more than one — "a comment stating two defaults cannot be attributed"
			// — and it went GREEN over KeelHardenedEnabled, whose doc comment says DEFAULT-OFF
			// and whose line comment says "(default FALSE)": TWO claims, the exact field this
			// rule was written for. A doc comment is attached to its field by the parser, so
			// attributing it is not a guess; the cost is that a comment which mentions a
			// NEIGHBOUR's default reds here and has to be reworded. That is the safe direction.
			checked++
			for _, c := range claims {
				if got := claimIsTrue(c[1]); got != want {
					t.Errorf("Config.%s: the comment says %q, and the loader gives %v when the "+
						"environment is unset — the comment is a claim about the line that reads "+
						"the variable, and it disagrees with it",
						name, strings.TrimSpace(c[0]), want)
				}
			}
		}
		return false
	})

	if checked < floorDefaultClaims {
		t.Fatalf("only %d fields carry a checkable default claim (floor %d) — the comment scan "+
			"matched nothing and this rule would pass over any stale claim", checked, floorDefaultClaims)
	}
}

// commentText returns a comment group as one line of prose, with the `//` markers
// REMOVED and every run of whitespace collapsed to a single space.
//
// ⚠ BOTH HALVES ARE THE REPAIR, AND THE FIRST CUT OF THIS FUNCTION HAD NEITHER. It
// joined c.Text — which still carries the leading `//` — with a space, so a claim that
// WRAPPED across a line break was scanned as `Default // false` and the separator class
// `[-: ]*` does not contain a slash. Not merely unmatched: unmatchable. Measured, that is
// exactly how DistillPoolableEnabled's "Default\n// false" survived the commit that
// corrected CachePoolableEnabled three lines above it.
//
// ast.CommentGroup.Text() is what strips the markers, and it is the right instrument
// rather than a hand-rolled TrimPrefix: it is the same text godoc renders, which is what
// this rule's attribution argument already leans on. It joins lines with "\n", so the
// whitespace collapse is what lets a claim be matched across the wrap it was written at.
func commentText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	return strings.Join(strings.Fields(g.Text()), " ")
}

// cfgFieldRef finds every `cfg.<Name>` in a chunk of text whose Name is a Config bool
// field. It is the ONLY binding this rule accepts, and the two it rejects were each
// rejected on a measurement rather than on taste — see the rule's doc comment.
var cfgFieldRef = regexp.MustCompile(`\bcfg\.([A-Za-z0-9_]+)\b`)

func boundFlags(text string, fields map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, m := range cfgFieldRef.FindAllStringSubmatch(text, -1) {
		if _, ok := fields[m[1]]; ok {
			out[m[1]] = true
		}
	}
	return out
}

// documentedLine returns the first line of real code a comment group sits above: the next
// non-blank line that is not itself a comment. Empty when the group documents nothing
// (a trailing block at end of file, or two groups separated by a blank line).
//
// ⚠ THIS IS HALF THE BINDING AND THE RULE IS BLIND WITHOUT IT. Measured: of the three
// wiring-site claims that were false at the commit that widened this file, only ONE named
// its flag inside the comment prose. The other two — the fail-closed settlement posture and
// the pattern-earning wire-up — name it only on the `if cfg.X {` line the comment is
// written about, which is exactly where a reader looks and where a text-only scan does not.
func documentedLine(src []string, endLine int) string {
	for i := endLine; i < len(src); i++ {
		s := strings.TrimSpace(src[i])
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "//") || strings.HasPrefix(s, "/*") {
			return ""
		}
		return s
	}
	return ""
}

// documentedStmt returns the source of the WHOLE declaration or statement a comment sits
// above, rather than its first line — the smallest node that starts on the line right after
// the group, taken whole.
//
// ⚠ WHY IT EXISTS: documentedLine reads EXACTLY ONE LINE, and a wire-up written as a
// multi-line call puts the flag out of its reach. Measured on the commit that added this:
// the settlement CLEARER is constructed over three lines and its only gate — `func() bool
// { return cfg.SettlementFailClosedEnabled }` — is on the THIRD. documentedLine returned
// `poolClearer := poolroyalty.NewSettlementClearer(`, which names no flag, so the comment
// above it went unbound and its "DEFAULT OFF" was never compared to anything. That is not
// a claim the prose binding could rescue either: it names the field BARE, and a bare name
// is the binding this rule rejected on five measured counterexamples.
//
// ⚠ IT IS APPLIED AS A FALLBACK, NOT A REPLACEMENT, AND THAT IS A MEASUREMENT NOT A
// PREFERENCE. Swapping documentedLine FOR this loses coverage: a statement is wider than a
// line, so it can pull in a SECOND cfg.<Field> and make an already-checked claim ambiguous.
// Measured over the tree, the straight swap bound 21 where the line bound 20 but turned
// FOUR checked claims into skips (main.go's keel wiring gained KeelHardenedEnabled beside
// KeelEnabled; econflags.go's two readouts lost theirs entirely). Used only when the line
// binding resolved NOTHING, it is strictly additive: +5 claims bound, 0 lost, 0 newly
// ambiguous. C3 in the control harness is the assertion that keeps it that way.
func documentedStmt(f *ast.File, fset *token.FileSet, raw []byte, g *ast.CommentGroup) string {
	best, bestEnd := token.NoPos, token.NoPos
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		switch n.(type) {
		case ast.Stmt, ast.Decl, ast.Spec:
		default:
			return true
		}
		if n.Pos() < g.End() {
			return true
		}
		// the smallest node starting earliest after the comment
		if best == token.NoPos || n.Pos() < best || (n.Pos() == best && n.End() < bestEnd) {
			best, bestEnd = n.Pos(), n.End()
		}
		return true
	})
	if best == token.NoPos {
		return ""
	}
	// ADJACENCY. A comment group that documents nothing — a trailing block at the end of a
	// function, or a note separated from what follows — must bind to nothing rather than to
	// whatever statement happens to come next.
	//
	// ⚠ MEASURED, AND IT IS NOT LOAD-BEARING TODAY — SAID HERE SO THE NEXT READER DOES NOT
	// CREDIT IT WITH WORK IT IS NOT DOING. C6 in the control harness deletes this clause and
	// the gate stays GREEN with the binding totals UNCHANGED (25 checked, 2 ambiguous). It is
	// not dead code — instrumented, it fires 25 times — but in all 25 the wider bind resolves
	// to NO cfg.<Field> anyway, so it is never decisive. It is kept as a bound on a scope that
	// is genuinely wider than documentedLine's (a statement can be a whole if-block, and
	// documentedLine has no distance limit of its own), not because anything here needs it.
	if fset.Position(best).Line-fset.Position(g.End()).Line > 1 {
		return ""
	}
	so, eo := fset.Position(best).Offset, fset.Position(bestEnd).Offset
	if so < 0 || eo > len(raw) || so >= eo {
		return ""
	}
	return string(raw[so:eo])
}

// TestDefaultClaimsAtTheWiringSitesMatchTheLoader — RULE H. The same falsifiable claim as
// the rule above, checked where the flag is USED rather than where it is declared.
//
// ⚠ WHY A SECOND RULE, AND WHY THE FIRST ONE COULD NOT JUST BE POINTED AT MORE FILES. The
// rule above attributes a comment to a field with the PARSER: a doc comment belongs to the
// field it is attached to, no heuristic involved. Outside config.go there is no field to
// attach to — a comment sits above an `if cfg.X {` or a wire-up call — so the subject has
// to be resolved from the text, and resolving it wrongly is how a guard starts accusing
// innocent code and gets weakened. The binding here is therefore the narrowest one that
// works: `cfg.<Field>`, a reference to the loaded struct itself.
//
// ⚠ THE TWO WIDER BINDINGS WERE TRIED AND MEASURED, NOT REJECTED ON TASTE:
//
//   - A LENS_* variable named in the prose: THREE false accusations. All three are the
//     poolable pair, where the comment states the default of the per-WORKSPACE column
//     (`cache_poolable` / `distill_poolable`, a row value) and names the GLOBAL flag's
//     environment variable in the very next clause — "Default false (private), so the
//     request path stays inert until an admin opts a workspace in here AND the global
//     LENS_CACHE_POOLABLE_ENABLED switch is on". Two subjects, one sentence; the env var
//     belongs to the one the claim is not about.
//   - A bare field name in the prose: TWO more. "so it is DEFAULT-OFF: this EconomyEnabled
//     block" is about the anti-gaming revoker, and "Default FALSE. Independent of
//     EconomyEnabled" is about the batch lane. In both the named flag is the neighbour the
//     claim distinguishes itself FROM.
//
// So a claim whose only nearby flag reference is an env var or a bare name is NOT checked.
// That is a real coverage hole and it is stated rather than hidden: at the widening commit
// 77 default claims across the repo name no cfg.<Field> at all and this rule cannot see one
// of them. It is the safe direction — the alternative is five accusations against correct
// comments, and a rule nobody trusts checks nothing.
//
// ⚠ WHAT IT CAUGHT, on the commit that widened it — three, all in cmd/lens/main.go, all
// about flags that are ARMED on a deployment whose environment says nothing, and every one
// stating the CONSEQUENCE backwards as well as the default:
//
//	KeelEnabled                 "DEFAULT-OFF (cfg.KeelEnabled) ... so a fresh deployment
//	                            records nothing" — it records.
//	SettlementFailClosedEnabled "Default OFF ⇒ 'held' (byte-identical)" — and the SAME FILE
//	                            says "DEFAULT-ON for the closed test" about the SAME flag
//	                            at the traffic-mint sweeper below it. One file, two answers.
//	PatternEarningEnabled       "Default off; flag-off the serve path is byte-identical to
//	                            capture-only" — the serve path is not byte-identical.
func TestDefaultClaimsAtTheWiringSitesMatchTheLoader(t *testing.T) {
	defaults := observedDefaults(t)
	if len(defaults) < floorDerivedDefaults {
		t.Fatalf("observed only %d bool fields on the loaded Config (floor %d) — the struct walk "+
			"read nothing and every comparison below would pass vacuously",
			len(defaults), floorDerivedDefaults)
	}

	selfPath, err := filepath.Abs(filepath.Join(".", configSource))
	if err != nil {
		t.Fatalf("abs %s: %v", configSource, err)
	}

	filesScanned, checked, ambiguous := 0, 0, 0
	walkErr := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "bin", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if abs == selfPath {
			return nil // the parser-attributed rule above owns this file
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, raw, parser.ParseComments)
		if err != nil {
			// A file this package cannot parse is not silently skipped: an unparseable
			// file is a file whose claims were never read.
			t.Errorf("parse %s: %v", path, err)
			return nil
		}
		filesScanned++
		src := strings.Split(string(raw), "\n")

		for _, g := range f.Comments {
			text := commentText(g)
			claims := defaultClaim.FindAllStringSubmatch(text, -1)
			if len(claims) == 0 {
				continue
			}
			end := fset.Position(g.End()).Line // 1-based; src is 0-based, so this indexes the line AFTER
			bound := boundFlags(text, defaults)
			for name := range boundFlags(documentedLine(src, end), defaults) {
				bound[name] = true
			}
			// FALLBACK, only where the line binding found nothing — see documentedStmt.
			if len(bound) == 0 {
				bound = boundFlags(documentedStmt(f, fset, raw, g), defaults)
			}
			if len(bound) != 1 {
				if len(bound) > 1 {
					ambiguous++
				}
				continue
			}
			var flag string
			for n := range bound {
				flag = n
			}
			checked++
			want := defaults[flag]
			for _, c := range claims {
				if got := claimIsTrue(c[1]); got != want {
					t.Errorf("%s:%d cfg.%s: the comment says %q, and the loader gives %v when the "+
						"environment is unset. This comment sits on the line that USES the flag, so "+
						"it is what a reader auditing this wiring believes about a default deployment",
						path, fset.Position(g.Pos()).Line, flag, strings.TrimSpace(c[0]), want)
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", repoRoot, walkErr)
	}

	if filesScanned < floorWiringFilesScanned {
		t.Fatalf("FLOOR: parsed %d non-test .go files (want >= %d) — the walk read almost nothing, "+
			"so a clean result here is the walk's silence and not the product's",
			filesScanned, floorWiringFilesScanned)
	}
	if checked < floorWiringClaims {
		t.Fatalf("FLOOR: bound only %d default claims to a cfg.<Field> (want >= %d) — the binding "+
			"resolved nothing and this rule would pass over every stale claim in the repo",
			checked, floorWiringClaims)
	}
	t.Logf("wiring-site default claims: %d files scanned, %d claims checked, %d skipped as ambiguous "+
		"(more than one cfg.<Field> in scope)", filesScanned, checked, ambiguous)
}
