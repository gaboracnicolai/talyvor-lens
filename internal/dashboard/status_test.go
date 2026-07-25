package dashboard

// The status page must be TRUE, then useful, then handsome — in that order.
//
// It replaced a 41-panel dashboard whose ten authenticated panels rendered a
// permanent em-dash because the page never sent a credential. The lesson is not
// "show fewer panels"; it is "show only what you can prove". So the assertions
// that matter most here are the NEGATIVE ones: no credentialed value, no
// aggregate that answers a question the visitor did not ask, and — the trap that
// produced this whole cleanup — no money constant copied out of Go with nothing
// to keep the copy honest.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var styleScriptRE = regexp.MustCompile(`(?is)<(style|script)\b.*?</(style|script)>`)

// stripStyleAndScript leaves only what a reader actually sees.
func stripStyleAndScript(s string) string { return styleScriptRE.ReplaceAllString(s, "") }

func render(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	New("0.1.0").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status page: got %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

/* ── What it must say ─────────────────────────────────────────────────────── */

func TestPageNamesTheServiceAndItsVersion(t *testing.T) {
	body := render(t)
	if !strings.Contains(body, "Talyvor Lens") {
		t.Error("the page must name the service")
	}
	if !strings.Contains(body, "0.1.0") {
		t.Error("the page must carry the version it was built with")
	}
}

// TestPageSaysItIsAnAPINotAUI: the whole reason a visitor is confused here is
// that they typed an API hostname expecting an app. Say so, first.
func TestPageSaysItIsAnAPINotAUI(t *testing.T) {
	body := strings.ToLower(render(t))
	if !strings.Contains(body, "api") {
		t.Error("the page must say this host is an API")
	}
	if !strings.Contains(body, "not a") && !strings.Contains(body, "isn’t") && !strings.Contains(body, "rather than") {
		t.Error("the page must distinguish itself from the app, not merely mention the word API")
	}
}

// TestPagePointsAtTheDetailedStatusPage: /status is a SEPARATE, pre-existing and
// fully working public page (internal/status) — per-component health, provider
// latencies, all unauthenticated and all real. A page whose job is to point
// onward must not omit the most useful destination on its own host.
func TestPagePointsAtTheDetailedStatusPage(t *testing.T) {
	if !strings.Contains(render(t), "/status") {
		t.Error("the page must link to /status, the detailed public health page on this same host")
	}
}

func TestPagePointsAtTheDashboardAndTheDocs(t *testing.T) {
	body := render(t)
	if !strings.Contains(body, "app.talyvor.com") {
		t.Error("the page must point at where the dashboard actually is")
	}
	if !strings.Contains(body, "docs.talyvor.com") {
		t.Error("the page must point at the docs")
	}
}

// TestPageReadsHealthFromTheUnauthenticatedEndpoint: /healthz already answers
// without a credential, so health is the ONE live reading this page can honestly
// show. It must actually fetch it rather than paint a decorative green dot.
func TestPageReadsHealthFromHealthz(t *testing.T) {
	body := render(t)
	if !strings.Contains(body, "/healthz") {
		t.Error("health must be read from /healthz — the endpoint that answers unauthenticated")
	}
	if !strings.Contains(body, "fetch(") {
		t.Error("health must be fetched live, not baked into the HTML")
	}
}

/* ── What it must NOT say ─────────────────────────────────────────────────── */

// TestPageShowsNoCredentialedData is the core regression guard. Every path below
// requires an API key; the page holds none, so requesting any of them could only
// ever render a 401 as an em-dash — the exact failure that emptied the old
// dashboard.
func TestPageShowsNoCredentialedData(t *testing.T) {
	body := render(t)
	credentialed := []string{
		"/v1/api/", "/v1/workspaces/", "/v1/api/catalog", "/v1/api/workspaces",
		"/v1/api/distill/summary", "/v1/api/routing/intelligence", "/v1/api/local/status",
		"/v1/api/anomalies/scan", "/v1/api/alerts/circuits", "/v1/api/povi/",
		"/v1/api/modality/capabilities",
	}
	for _, p := range credentialed {
		if strings.Contains(body, p) {
			t.Errorf("the page references %s, which requires an API key it does not have — "+
				"it could only ever render a 401", p)
		}
	}
}

// TestPageShowsNoEconomyAggregates: /v1/economy/stats, /v1/oracle/stats and
// /v1/tokens/rates ARE public, so showing them would leak nothing. They are
// excluded on editorial grounds: they are global rather than the visitor's, they
// exist only when the economy is enabled (so the page would differ per
// deployment), and on a young install "circulating supply 2,000 µLENS · 0 oracle
// tasks" reproduces the exact "this product is dead" impression this page was
// built to correct — with accurate data. They remain curl-able; they are not
// front-door material.
func TestPageShowsNoEconomyAggregates(t *testing.T) {
	body := render(t)
	for _, p := range []string{"/v1/economy/", "/v1/oracle/stats", "/v1/tokens/rates", "/v1/marketplace/"} {
		if strings.Contains(body, p) {
			t.Errorf("the page reads %s — public, but not this page's business", p)
		}
	}
	for _, w := range []string{"circulating", "total supply", "Total Supply"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(w)) {
			t.Errorf("the page renders %q", w)
		}
	}
}

// TestPageHardcodesNoMoneyConstants is the guard for the defect that survived the
// old dashboard: the LXC peg was hardcoded as "$0.10" twice and the staking APYs
// as 5/12/20% three times, duplicating economy.LXCUSDValue and marketplace.APY*
// with NOTHING linking the copies to their source. All five were correct on the
// day they were written — that is what makes the shape dangerous. This page
// carries no money constant at all, and this test fails if one appears.
func TestPageHardcodesNoMoneyConstants(t *testing.T) {
	// Check what a READER sees: CSS legitimately contains "100%" and script
	// legitimately contains numbers, and neither is a money constant. Stripping
	// them is what makes this assertion mean what it says.
	body := stripStyleAndScript(render(t))
	// Any currency amount, any percentage — on a page that has no business
	// quoting either.
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`\$\s?\d`),
		regexp.MustCompile(`\d+\s?%`),
		regexp.MustCompile(`(?i)\bAPY\b`),
		regexp.MustCompile(`(?i)\bpeg\b`),
		regexp.MustCompile(`(?i)\bLXC\b`),
		regexp.MustCompile(`(?i)\bLENS\b(?i)\s*(token|per|=)`),
	} {
		if m := re.FindString(body); m != "" {
			t.Errorf("the page contains %q (matched %s) — a money constant copied from Go with "+
				"nothing to keep the copy honest is how the old dashboard went stale", m, re)
		}
	}
}

/* ── How it must look ─────────────────────────────────────────────────────── */

// TestPageUsesTheSuiteTokens pins the page to the design system's SHIPPED values
// (talyvor-suite packages/ui/src/theme.css), not to an approximation. Drift here
// means the API host stops looking like the product.
func TestPageUsesTheSuiteTokens(t *testing.T) {
	body := render(t)
	shipped := map[string]string{
		"--canvas light":  "#F4F5F6",
		"--surface light": "#FFFFFF",
		"--ink light":     "#1B1D1F",
		"--muted light":   "#6B6E73",
		"--accent light":  "#0B7A85",
		"--canvas dark":   "#141618",
		"--surface dark":  "#1D2023",
		"--ink dark":      "#EDEFF1",
		"--accent dark":   "#3ABDC9",
	}
	for name, hex := range shipped {
		if !strings.Contains(body, hex) {
			t.Errorf("%s: %s missing — tokens must come from the suite's shipped theme.css", name, hex)
		}
	}
	if !strings.Contains(body, "-apple-system") {
		t.Error("the system font stack from the suite must be used (macOS System Settings reference)")
	}
	if !strings.Contains(body, "prefers-color-scheme") {
		t.Error("the page must honour the viewer's light/dark preference, as the suite does")
	}
	if !strings.Contains(body, "prefers-reduced-motion") {
		t.Error("the suite's reduced-motion floor must be honoured")
	}
}

// TestPageIsSelfContained: no CDN, no external font, no analytics. The API host
// must not make a visitor's browser talk to a third party.
func TestPageIsSelfContained(t *testing.T) {
	body := render(t)
	for _, bad := range []string{"http://", "//cdn", "googleapis", "unpkg", "jsdelivr", "<script src"} {
		if strings.Contains(body, bad) {
			t.Errorf("the page references %q — it must be entirely self-contained", bad)
		}
	}
}

func TestPageEscapesNothingDynamicIntoHTML(t *testing.T) {
	// The version is the only interpolation; prove a hostile value cannot break out.
	rec := httptest.NewRecorder()
	New(`"><script>alert(1)</script>`).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), "<script>alert(1)</script>") {
		t.Error("the version string was interpolated unescaped — an injection")
	}
}
