package api

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// openapi_route_contract_test.go — the served OpenAPI document against the routes the binary
// actually registers.
//
// openapi.go is not a README. It is SERVED at GET /openapi.json, which means a client generates
// against it. W6.15 (#483) compared one route's RESPONSE BODY with components.schemas.LocalEndpoint
// and found it shipping four undeclared fields. That was one route and one schema; nobody had
// compared the PATH LIST.
//
// ⚠ THE TWO DIRECTIONS ARE NOT THE SAME KIND OF FACT, and this file treats them differently.
//
//	PUBLISHED BUT NOT REGISTERED   unambiguous. A client reads the contract, calls it, gets 404.
//	                               Measured at ZERO today, and guarded so it stays there.
//	REGISTERED BUT NOT PUBLISHED   a judgement. 122 non-admin /v1 routes are outside the document
//	                               and openapi.go's own header says why: "Lens has too many routes
//	                               to spec every single one". Counted and classified, NOT called a
//	                               defect — reporting it as one would be manufacturing a finding.
//
// ⚠ AND A FALSE FINDING GOT THIS FAR BEFORE BEING CAUGHT. The first run reported
// `POST /v1/proxy/openai/{path}` and `/v1/proxy/anthropic/{path}` as published-but-unregistered.
// They are registered — as `/v1/proxy/openai/*`. chi spells a wildcard `*`; OpenAPI spells it
// `{path}`. Two confident, serious-sounding, entirely false findings from a spelling difference,
// and it is the flattering direction. normalisePath below is what makes the comparison honest, and
// TestNormalisationCollapsesChiWildcardsAndOpenAPIParams is what keeps it that way.

// registration matches a chi route registration in main.go. The `With(...)` clause is optional and
// carries middleware, not path.
//
// ⚠ THE LEADING `/` IN THE PATH GROUP IS LOAD-BEARING. Without it this matches `q.Get("model")` —
// url.Values.Get, not a route — and the scoping count for W6.29 was inflated by 13 before that was
// noticed. A route census that counts query-parameter reads as routes overstates coverage.
var registration = regexp.MustCompile(`\b(\w+)\.(?:With\([^)]*\)\.)?(Get|Post|Put|Delete|Patch|Head|Options)\("(/[^"]*)"`)

var pathParam = regexp.MustCompile(`\{[^}]*\}`)

// normalisePath makes a chi path and an OpenAPI path comparable: parameter NAMES are not part of
// the route's identity, and chi's trailing `/*` is the same thing as OpenAPI's `/{path}`.
func normalisePath(p string) string {
	p = strings.ReplaceAll(p, "/*", "/{}")
	return pathParam.ReplaceAllString(p, "{}")
}

func repoRootForAPI(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("root %s has no go.mod", root)
	}
	return root
}

// registeredRoutes returns the set of "METHOD /normalised/path" the binary registers.
//
// ⚠ IT READS main.go's SOURCE, AND THE REASON IS RECORDED RATHER THAN GLOSSED. chi.Walk over the
// real router would be better — it would see what the process serves. It is not reachable: the
// router is built inline inside run()'s full dependency graph, so there is no exported seam that
// returns a mounted router. cmd/lens/admin_route_classification_test.go states the same limit and
// this file inherits it rather than pretending otherwise.
//
// ⚠ MEASURED BEFORE RELYING ON IT: main.go contains no `.Route(` and no `.Mount(`, so every path
// literal at a registration site IS the served path. TestNoMountOrRouteHidesAPrefix asserts that,
// because the day one appears every comparison here silently mis-attributes.
func registeredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(repoRootForAPI(t), "cmd", "lens", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	out := map[string]bool{}
	for _, m := range registration.FindAllStringSubmatch(string(src), -1) {
		// ⚠ ASSERTED, NOT ASSUMED. Control W4 removed the leading `/` from the regex and NOTHING
		// failed: the bogus entries it admits (`q.Get("model")` — url.Values.Get, not a route)
		// never start with /v1, so every downstream filter silently dropped them and the comment
		// claiming the guard was load-bearing was describing a discipline no assertion held. It
		// holds now.
		if !strings.HasPrefix(m[3], "/") {
			t.Errorf("parsed %q as a route path from `%s.%s(...)`. It is not a path — the most "+
				"likely culprit is url.Values.Get, which shares the method name and would inflate "+
				"any coverage count built on this parse.", m[3], m[1], m[2])
			continue
		}
		out[strings.ToUpper(m[2])+" "+normalisePath(m[3])] = true
	}
	if len(out) < 100 {
		t.Fatalf("parsed %d routes from main.go — the regex is not matching and every comparison "+
			"below would report a clean contract over nothing", len(out))
	}
	return out
}

// publishedOps returns "METHOD /normalised/path" for every operation in the served document.
func publishedOps(t *testing.T) map[string]bool {
	t.Helper()
	paths, ok := OpenAPISpec()["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatal("the served document declares no paths — an empty contract is trivially kept")
	}
	out := map[string]bool{}
	for p, v := range paths {
		ops, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("path %q has no operations map", p)
		}
		for method := range ops {
			out[strings.ToUpper(method)+" "+normalisePath(p)] = true
		}
	}
	return out
}

// ⚠ THE ONE UNAMBIGUOUS DIRECTION. Zero today; this is what keeps it zero.
func TestEveryPublishedPathIsRegistered(t *testing.T) {
	reg, pub := registeredRoutes(t), publishedOps(t)
	if len(pub) < 10 {
		t.Fatalf("only %d published operations — too few for this comparison to mean anything", len(pub))
	}
	var ghosts []string
	for op := range pub {
		if !reg[op] {
			ghosts = append(ghosts, op)
		}
	}
	sort.Strings(ghosts)
	if len(ghosts) > 0 {
		t.Errorf("%d operation(s) are published at GET /openapi.json and registered nowhere:\n  %s\n\n"+
			"A client generates against this document. A path in it that the binary does not serve "+
			"is a 404 the contract promised would work.",
			len(ghosts), strings.Join(ghosts, "\n  "))
	}
	t.Logf("MEASURED: %d published operations, all registered; %d routes registered in total.",
		len(pub), len(reg))
}

// ⚠ THE PREMISE THE PATH COMPARISON RESTS ON. A `.Route(` or `.Mount(` would mean the literal at a
// registration site is not the served path, and every result above becomes mis-attributed —
// silently, and in both directions.
func TestNoMountOrRouteHidesAPrefix(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRootForAPI(t), "cmd", "lens", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	for _, bad := range []string{".Route(", ".Mount("} {
		if strings.Contains(string(src), bad) {
			t.Errorf("main.go now uses %s. Route registrations are no longer served at the literal "+
				"path written beside them, so registeredRoutes() mis-attributes every route under "+
				"the prefix — resolve it before trusting anything in this file.", bad)
		}
	}
}

// ⚠ THE NORMALISER IS THE THING THAT NEARLY PRODUCED A FALSE FINDING, so it gets its own test.
func TestNormalisationCollapsesChiWildcardsAndOpenAPIParams(t *testing.T) {
	cases := []struct{ chi, oai string }{
		{"/v1/proxy/openai/*", "/v1/proxy/openai/{path}"},
		{"/v1/workspaces/{wsID}/config", "/v1/workspaces/{workspace_id}/config"},
		{"/v1/api/keys/{keyID}", "/v1/api/keys/{id}"},
	}
	for _, c := range cases {
		if normalisePath(c.chi) != normalisePath(c.oai) {
			t.Errorf("%q and %q normalise differently (%q vs %q) — they are the same route, and a "+
				"comparison that says otherwise reports a confident false finding",
				c.chi, c.oai, normalisePath(c.chi), normalisePath(c.oai))
		}
	}
	// And it must still DISCRIMINATE: a normaliser that collapsed everything would hide every
	// genuinely missing path.
	if normalisePath("/v1/a/{x}") == normalisePath("/v1/b/{x}") {
		t.Error("normalisePath collapses distinct paths — it would hide every real ghost")
	}
	if normalisePath("/v1/a/{x}") == normalisePath("/v1/a/{x}/b") {
		t.Error("normalisePath ignores path depth")
	}
}

// ── the header's own claim ──────────────────────────────────────────────────────────────────────

// declaredSurfaces are the surfaces openapi.go's header says this document covers, each with the
// pattern a published path for it would match.
//
// ⚠ THIS IS THE FALSIFIABLE PART OF A COMMENT NOBODY HAD CHECKED. The header names what the file
// covers; a named surface with no published path is a claim the document does not keep, and it is
// the kind a reader has no way to test short of reading every path.
var declaredSurfaces = []struct{ name, match string }{
	{"proxy endpoints", "/v1/proxy/"},
	{"key management", "/v1/api/keys"},
	{"workspaces", "/v1/workspaces/"},
	{"tenant config", "/config"},
	{"local-endpoint registry", "/v1/local/endpoints"},
}

func TestTheHeaderNamesOnlySurfacesTheDocumentCovers(t *testing.T) {
	paths, _ := OpenAPISpec()["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("no published paths")
	}
	for _, s := range declaredSurfaces {
		var found bool
		for p := range paths {
			if strings.Contains(p, s.match) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("openapi.go's header says this document covers %q and no published path "+
				"matches %q. A named surface with nothing behind it is a promise a generated "+
				"client cannot fulfil.", s.name, s.match)
		}
	}

	// ⚠ THE OTHER HALF, AND THE ONE THAT MAKES THIS NON-TAUTOLOGICAL. The surface list must be
	// checked against the header TEXT, or someone can satisfy this test by deleting a surface from
	// the list while leaving the header claiming it.
	src, err := os.ReadFile("openapi.go")
	if err != nil {
		t.Fatalf("read openapi.go: %v", err)
	}
	header := string(src)
	if i := strings.Index(header, "func OpenAPISpec"); i > 0 {
		header = header[:i]
	}
	for _, s := range declaredSurfaces {
		if !strings.Contains(header, s.name) {
			t.Errorf("declaredSurfaces lists %q and the header does not mention it — the list has "+
				"drifted from the claim it is checking", s.name)
		}
	}
	// ⚠ THE OMISSIONS MUST BE NAMED, AND NAMED ON THE RIGHT SIDE OF THE LINE. A word-ban would
	// stop the header ever DISCUSSING what it leaves out, which is the opposite of the point. So
	// the check is positional: `attribution` and `A/B` may appear only AFTER the NOT COVERED
	// marker. Claiming them as covered puts them before it and fails.
	const marker = "NOT COVERED"
	cut := strings.Index(header, marker)
	if cut < 0 {
		t.Fatalf("openapi.go's header has no %q section. This document covers 12 of 147 routes; a "+
			"header that lists only what it has is read as a complete contract.", marker)
	}
	claimed, omitted := header[:cut], header[cut:]
	for _, name := range []string{"attribution", "A/B"} {
		if strings.Contains(claimed, name) {
			t.Errorf("openapi.go's header claims to cover %q before the %q marker. Measured: ZERO "+
				"published paths for it, against 8 registered attribution routes and 6 "+
				"experiment/eval-A-B routes outside /v1/admin.", name, marker)
		}
		if !strings.Contains(omitted, name) {
			t.Errorf("openapi.go's header does not record %q as uncovered. An omission nobody wrote "+
				"down is indistinguishable from one nobody noticed.", name)
		}
	}
}

// ── the undocumented surface, counted and classified rather than called a defect ────────────────

// ⚠ 122 IS RECORDED SO IT CANNOT DRIFT IN SILENCE, NOT SO IT CAN BE DRIVEN TO ZERO. openapi.go says
// "Lens has too many routes to spec every single one" and that is a reasonable position for an
// admin-heavy API. What is NOT reasonable is the number changing by fifty without anyone noticing
// which way, so the count is pinned with a tolerance and its message says what each direction means.
const undocumentedNonAdminV1Routes = 122

func TestUndocumentedNonAdminRouteCountIsRecorded(t *testing.T) {
	reg, pub := registeredRoutes(t), publishedOps(t)
	n := 0
	for op := range reg {
		if pub[op] {
			continue
		}
		p := strings.SplitN(op, " ", 2)[1]
		if strings.HasPrefix(p, "/v1/admin/") || !strings.HasPrefix(p, "/v1/") {
			continue
		}
		n++
	}
	if n != undocumentedNonAdminV1Routes {
		t.Errorf("%d non-admin /v1 routes are outside the published contract; W6.29 measured %d.\n"+
			"    FEWER means somebody documented routes — good, and worth saying which.\n"+
			"    MORE means the tenant API grew and the contract did not. Neither is automatically "+
			"wrong; both should be deliberate.", n, undocumentedNonAdminV1Routes)
	}
}
