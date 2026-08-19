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

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

const configSource = "config.go"

// FLOORS. Both halves below are comparisons over a parsed set, and a comparison over an empty
// set passes. Measured at the commit that introduced this file by raising each floor above
// reality and reading what it reported: 60 bool fields on the loaded Config, 43 of them
// carrying a default claim in a comment.
const (
	floorDerivedDefaults = 45
	floorDefaultClaims   = 15
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

func commentText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	var parts []string
	for _, c := range g.List {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, " ")
}
