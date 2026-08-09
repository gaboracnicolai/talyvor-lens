package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// admin_route_classification_test.go — EVERY /v1/admin ROUTE IS CLASSIFIED, FROM THE ROUTER SOURCE.
//
// W1.3 gives the suite BFF a Lens credential that can perform the cross-tenant ADMIN READS the
// operator screen needs. The dangerous half of that change is not the credential — it is the
// question "which routes does it reach", because the answer has to stay right as routes are added.
//
// ⚠ WHY THE INPUT IS THE SOURCE AND NOT A HAND-WRITTEN LIST. A curated list is already wrong here:
// `grep '"/v1/admin'` over main.go finds 25 registrations, but the real path count is 28 — four of
// the held-mint revocation routes are built in a LOOP from a string slice. A list someone typed
// would omit them, and an omitted route defaults into "nobody classified this", which is exactly
// how a write route ends up reachable. So this file parses main.go, expands that loop from its own
// slice literal, and REFUSES TO PASS if any path it finds is absent from the tables below.
//
// ⚠ WHY `chi.Walk` IS NOT USED. It would be better — it would see the routes the process actually
// serves rather than the text that registers them. It is not reachable from a test: the router is
// built inline inside run()'s full dependency graph (a live pool, redis, nats, ~40 constructors),
// so there is no exported seam that returns a mounted router. Saying that plainly beats shipping a
// walk over an empty router, which would pass while covering nothing.

// operatorReadable — the routes LENS_OPERATOR_READ_KEY may GET.
//
// The bar for membership, applied per route below: it returns DESCRIPTIVE data, it writes no row,
// it moves no money, and the operator screen (W1.4: workspace, spend, purchases, held, last
// activity) or its audit story actually needs it.
var operatorReadable = map[string]string{
	"/v1/admin/economy/flags":             "the live value of every economy flag as the process holds it. Observation of config; econflags.Handler 405s a non-GET on its own.",
	"/v1/admin/keel/findings":             "recorded drift findings. keel.Reader is Query-only.",
	"/v1/admin/keel/haircuts":             "applied drift haircuts, already-happened facts read back.",
	"/v1/admin/output-verdicts":           "forensic verdict read across workspaces.",
	"/v1/admin/workspaces":                "the tenant roster — the first column of the operator table. In-memory workspace.Manager, no DB write path.",
	"/v1/admin/routing-decisions/summary": "override rate + estimated cost delta. The estimate is descriptive, not money.",
	"/v1/admin/distill/attribution":       "distill attribution rows. distillattrib.Reader is Query-only by construction.",
	"/v1/admin/billing/purchases":         "the purchases column of the operator table.",
	"/v1/admin/pool-royalty/detect":       "self-dealing detection read. Query-only reader, explicitly NOT economy-gated so forensics survive the kill switch.",
	"/v1/admin/pool-royalty/resolve":      "the resolution view of the same detection. Query-only.",
	"/v1/admin/pool-royalty/margin":       "margin observability. Query-only.",
	"/v1/admin/worktier/distribution":     "descriptive tier distribution for ONE workspace. Money-decoupled; never feeds mint/earn/billing.",
	"/v1/admin/distill-royalty/detect":    "the distill mirror of pool-royalty/detect. Query-only.",
	"/v1/admin/distill-royalty/resolve":   "the distill mirror of pool-royalty/resolve. Query-only.",
	"/v1/admin/distill-royalty/margin":    "the distill mirror of pool-royalty/margin. Query-only.",
}

// operatorMustNotReach — everything else, each with the reason it is out.
//
// ⚠ THE FIRST SEVEN ARE NAMED BY THE BRIEF ITSELF and are asserted individually below, so deleting
// a line here cannot quietly widen the credential.
var operatorMustNotReach = map[string]string{
	"/v1/admin/lxc/grant":                                        "MINTS LXC. Moves money.",
	"/v1/admin/conversion-rate/approve":                          "CHANGES A PRICE.",
	"/v1/admin/bonds/{bond_id}/settle":                           "slashes or releases a provenance bond. Moves money.",
	"/v1/admin/distill-royalty/adjudicate":                       "REVOKES A PAYOUT.",
	"/v1/admin/pool-royalty/adjudicate":                          "REVOKES A PAYOUT.",
	"/v1/admin/held-mints/traffic/adjudicate":                    "REVOKES A PAYOUT.",
	"/v1/admin/annotation-reputation/reset":                      "writes a reputation event. A write is a write even when it is not money.",
	"/v1/admin/held-mints/eval_contribution_mints/adjudicate":    "REVOKES A PAYOUT (loop-generated).",
	"/v1/admin/held-mints/routing_prediction_mints/adjudicate":   "REVOKES A PAYOUT (loop-generated).",
	"/v1/admin/held-mints/node_latency_mints/adjudicate":         "REVOKES A PAYOUT (loop-generated).",
	"/v1/admin/held-mints/confidential_compute_mints/adjudicate": "REVOKES A PAYOUT (loop-generated).",

	// ⚠ MEASURED, AND THE PROVISIONAL CLASSIFICATION WAS WRONG. The W1.3 groundwork report listed
	// attest/{output_id} as provisionally READ because it is registered with r.Handle and reads
	// like a lookup. It is not: newAttestHandler runs a sandboxed COMPILE of an uploaded source
	// tree (a 128 MiB request body) and its result carries `recorded` — it WRITES a
	// talyvor_verified verdict attributed to the output's owner. It is a write route with a
	// read-shaped name.
	"/v1/admin/attest/{output_id}": "WRITES a talyvor_verified verdict from an uploaded 128 MiB source tree. Read-shaped name, write behaviour — measured, not assumed.",

	// ⚠ TRAP 3, MEASURED. "preview" reads like a read and the verb says write. It persists nothing
	// (no model call, no token_events, no spend — it is a dry run), so the provisional table parked
	// it in OUT pending measurement. It STAYS OUT, for a reason the dry-run framing hides: it feeds
	// an attacker-supplied document to distill.ProcessIsolator, a 512 MiB subprocess. A credential
	// that can spend server CPU and memory on uploaded files is not a read credential. It is also
	// gated by its own inline IsAdmin closure rather than requireAdmin, so it was never a candidate.
	"/v1/admin/distill/preview": "POST that spawns a 512 MiB converter subprocess over an uploaded document. Persists nothing, but consuming compute on attacker-supplied bytes is not a read.",
}

// adminPathRe finds every literal /v1/admin path in the router source.
var adminPathRe = regexp.MustCompile(`"(/v1/admin[^"]*)"`)

// heldMintLoopRe finds the slice literal the four revocation routes are generated from, so the
// enumeration expands with the code instead of going stale against it.
var heldMintLoopRe = regexp.MustCompile(`for\s+_,\s*mt\s*:=\s*range\s*\[\]string\{([^}]*)\}`)

func readMainGo(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	return string(raw)
}

// adminRoutesFromSource returns every /v1/admin path the router registers, with the loop expanded.
func adminRoutesFromSource(t *testing.T) map[string]bool {
	t.Helper()
	src := readMainGo(t)

	// The loop's mint types, parsed from its own slice literal.
	var mintTypes []string
	if m := heldMintLoopRe.FindStringSubmatch(src); m != nil {
		for _, part := range strings.Split(m[1], ",") {
			if v := strings.Trim(strings.TrimSpace(part), `"`); v != "" {
				mintTypes = append(mintTypes, v)
			}
		}
	}
	if len(mintTypes) == 0 {
		t.Fatal("the held-mints loop's []string literal was not found in main.go — the expansion " +
			"is broken, and four adjudicate routes would silently classify as non-existent")
	}

	paths := map[string]bool{}
	for _, m := range adminPathRe.FindAllStringSubmatch(src, -1) {
		p := m[1]
		// The loop's fragment: `"/v1/admin/held-mints/"+mt+"/adjudicate"` yields the prefix.
		// Expand it into the concrete four rather than classifying a prefix.
		if p == "/v1/admin/held-mints/" {
			for _, mt := range mintTypes {
				paths[p+mt+"/adjudicate"] = true
			}
			continue
		}
		paths[p] = true
	}
	return paths
}

// TestEveryAdminRouteIsClassified — the guard proper. A route nobody classified is a route whose
// reachability nobody decided.
func TestEveryAdminRouteIsClassified(t *testing.T) {
	routes := adminRoutesFromSource(t)

	// Non-vacuity: the groundwork enumeration counted 28. Fewer means the parse broke, and a guard
	// that enumerates nothing passes everything.
	if len(routes) < 28 {
		t.Fatalf("only %d /v1/admin paths parsed from main.go, want >= 28 — the enumeration is "+
			"broken. Found: %v", len(routes), sortedKeys(routes))
	}

	for p := range routes {
		_, read := operatorReadable[p]
		_, out := operatorMustNotReach[p]
		switch {
		case read && out:
			t.Errorf("%s is in BOTH tables — it cannot be both reachable and unreachable", p)
		case !read && !out:
			t.Errorf("%s is registered by the router and classified by NEITHER table.\n"+
				"    Add it to operatorReadable (with why a read credential may GET it) or to "+
				"operatorMustNotReach (with why it must not). Leaving it out is not neutral: an "+
				"unclassified route is one nobody decided about.", p)
		}
	}
}

// TestClassificationTablesHaveNoGhosts — the other direction. A table entry naming a route the
// router no longer registers is a line that guards nothing and reads as though it does.
func TestClassificationTablesHaveNoGhosts(t *testing.T) {
	routes := adminRoutesFromSource(t)
	for _, table := range []struct {
		name string
		m    map[string]string
	}{
		{"operatorReadable", operatorReadable},
		{"operatorMustNotReach", operatorMustNotReach},
	} {
		for p := range table.m {
			if !routes[p] {
				t.Errorf("%s names %s, which the router does not register. Stale entry — remove it, "+
					"or fix the path.", table.name, p)
			}
		}
	}
}

// TestEveryClassificationCarriesAReason — an entry with an empty reason is a decision nobody made.
func TestEveryClassificationCarriesAReason(t *testing.T) {
	for _, table := range []struct {
		name string
		m    map[string]string
	}{
		{"operatorReadable", operatorReadable},
		{"operatorMustNotReach", operatorMustNotReach},
	} {
		for p, why := range table.m {
			if len(strings.TrimSpace(why)) < 10 {
				t.Errorf("%s[%s] has no reason. Every entry states why, so the next person can "+
					"disagree with a decision rather than guess at one.", table.name, p)
			}
		}
	}
}

// ⚠ THE WIRING PROOF. The tables above are prose until something checks the ROUTER agrees with
// them. This reads each route's registration site and asserts the gate actually used there:
// a readable route must be wrapped in requireAdminOrOperatorRead, and an unreachable one must NOT
// be. Without this, moving a path between the tables would change a comment and nothing else.
func TestClassificationMatchesTheGateAtEachRegistrationSite(t *testing.T) {
	lines := strings.Split(readMainGo(t), "\n")

	for i, line := range lines {
		matches := adminPathRe.FindAllStringSubmatch(line, -1)
		if len(matches) == 0 {
			continue
		}
		// The gate can sit on the registration line or the line(s) that continue the call.
		window := strings.Join(lines[i:min(i+3, len(lines))], "\n")
		gated := strings.Contains(window, "requireAdminOrOperatorRead(")

		for _, m := range matches {
			p := m[1]
			if p == "/v1/admin/held-mints/" {
				// Loop-generated adjudicate routes; all four are OUT and none may carry the gate.
				if gated {
					t.Errorf("the held-mints adjudicate loop at main.go:%d is wrapped in "+
						"requireAdminOrOperatorRead — four PAYOUT REVOCATION routes just became "+
						"reachable by a read credential", i+1)
				}
				continue
			}
			if _, ok := operatorReadable[p]; ok {
				if !gated {
					t.Errorf("main.go:%d registers %s, classified operatorReadable, but its gate is "+
						"not requireAdminOrOperatorRead — the classification says reachable and the "+
						"router says otherwise", i+1, p)
				}
				continue
			}
			if _, ok := operatorMustNotReach[p]; ok && gated {
				t.Errorf("main.go:%d registers %s, classified operatorMustNotReach, and it IS "+
					"wrapped in requireAdminOrOperatorRead. %s", i+1, p, operatorMustNotReach[p])
			}
		}
	}
}

// ⚠ THE BRIEF'S OWN LIST, ASSERTED ONE BY ONE. W1.3 names these as "each its own assertion" —
// a loop over a table would go green if the table lost a row.
func TestBriefNamedRoutesAreOut(t *testing.T) {
	for _, p := range []string{
		"/v1/admin/lxc/grant",
		"/v1/admin/conversion-rate/approve",
		"/v1/admin/bonds/{bond_id}/settle",
		"/v1/admin/distill-royalty/adjudicate",
		"/v1/admin/held-mints/traffic/adjudicate",
		"/v1/admin/annotation-reputation/reset",
	} {
		if _, ok := operatorReadable[p]; ok {
			t.Errorf("%s is in operatorReadable. The brief names it as must-stay-unreachable: "+
				"anything that moves money, revokes a payout or changes a price is OUT.", p)
		}
		if _, ok := operatorMustNotReach[p]; !ok {
			t.Errorf("%s is not in operatorMustNotReach. The brief names it explicitly; dropping "+
				"the row would let TestEveryAdminRouteIsClassified be the only thing standing "+
				"between it and reach.", p)
		}
	}
	// The seventh is not under /v1/admin, so it is asserted here rather than in the tables.
	src := readMainGo(t)
	if idx := strings.Index(src, `"/v1/api/injection/patterns"`); idx >= 0 {
		window := src[idx:min(idx+400, len(src))]
		if strings.Contains(window, "requireAdminOrOperatorRead(") {
			t.Error("/v1/api/injection/patterns is wrapped in requireAdminOrOperatorRead — it " +
				"mutates the process-wide injection detector and accepts arbitrary regex")
		}
	} else {
		t.Error(`"/v1/api/injection/patterns" is not in main.go — the brief names it as ` +
			`must-stay-unreachable and this assertion has drifted from the code`)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
