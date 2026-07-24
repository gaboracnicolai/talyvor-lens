package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/dashboard"
)

// U3 master economy kill-switch tests. The adversarial setup throughout:
// LENS_ECONOMY_ENABLED=false while every individual economy gate is forced ON —
// the master must win.

func setRequiredEnv(t *testing.T) {
	t.Setenv("LENS_REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("LENS_DATABASE_URL", "postgres://localhost:5432/lens")
	t.Setenv("LENS_NATS_URL", "nats://localhost:4222")
	t.Setenv("LENS_OPENAI_API_KEY", "sk-test")
	t.Setenv("LENS_ANTHROPIC_API_KEY", "sk-ant-test")
}

// the 12 economy gate env vars (force-ON for the adversarial test).
var economyGateEnv = []string{
	"LENS_PATTERN_MINING_ENABLED", "LENS_PATTERN_CAPTURE_ENABLED", "LENS_PATTERN_EARNING_ENABLED",
	"LENS_POOL_ROYALTY_MINTING_ENABLED", "LENS_POVI_MINTING_ENABLED", "LENS_TRUSTFUL_COMPUTE_MINT_ENABLED",
	"LENS_CACHE_SHARING_ENABLED", "LENS_CACHE_POOLABLE_ENABLED", "LENS_DISTILL_POOLABLE_ENABLED",
	"LENS_LXC_GATING_ENABLED", "LENS_LXC_SHADOW_SPEND_ENABLED", "LENS_ROUTING_INTELLIGENCE_ENABLED",
	"LENS_EVAL_CONTRIBUTION_MINTING_ENABLED", "LENS_LATENCY_MINTING_ENABLED", "LENS_CONFIDENTIAL_MINTING_ENABLED",
	"LENS_ANNOTATION_MINTING_ENABLED",
}

// TestEconomyKillSwitch_ForcesAllGatesOff — master off + all 12 gates env-true ⇒
// every effective gate is false. This is the core proof; reverting the force-off
// block in config.Load makes it red.
func TestEconomyKillSwitch_ForcesAllGatesOff(t *testing.T) {
	setRequiredEnv(t)
	for _, e := range economyGateEnv {
		t.Setenv(e, "true")
	}
	t.Setenv("LENS_ECONOMY_ENABLED", "false")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EconomyEnabled {
		t.Fatal("EconomyEnabled should be false")
	}
	// The 10 ECONOMY gates force OFF (LXC is NOT here — it's fiat, U18).
	checks := map[string]bool{
		"PatternMining": cfg.PatternMiningEnabled, "PatternCapture": cfg.PatternCaptureEnabled,
		"PatternEarning": cfg.PatternEarningEnabled, "PoolRoyaltyMinting": cfg.PoolRoyaltyMintingEnabled,
		"POVIMinting": cfg.POVIMintingEnabled, "TrustfulComputeMint": cfg.TrustfulComputeMintEnabled,
		"CacheSharing": cfg.CacheSharingEnabled, "CachePoolable": cfg.CachePoolableEnabled,
		"DistillPoolable": cfg.DistillPoolableEnabled, "RoutingIntelligence": cfg.RoutingIntelligenceEnabled,
		"RoutingTierCohorts": cfg.RoutingTierCohortsEnabled,
		// P-o-I instance 1: the proof-of-eval-contribution EARNING gate (mints LENS) — force-off with the economy.
		"EvalContributionMinting": cfg.EvalContributionMintingEnabled,
		// P-o-I instance 2: the proof-of-routing-prediction EARNING gate (mints LENS) — force-off with the economy.
		"RoutingPredictionMinting": cfg.RoutingPredictionMintingEnabled,
		// P-o-I instance 3: the proof-of-latency-locality EARNING gate (mints LENS) — force-off with the economy.
		"LatencyMinting": cfg.LatencyMintingEnabled,
		// P-o-I instance 4: the proof-of-confidential-compute EARNING gate (mints LENS) — force-off with the economy.
		"ConfidentialMinting": cfg.ConfidentialMintingEnabled,
		// The annotation mint (spendable-immediate LENS) — force-off with the economy master switch.
		"AnnotationMinting": cfg.AnnotationMintingEnabled,
	}
	if len(checks) != 16 {
		t.Fatalf("expected 16 economy gates, got %d", len(checks))
	}
	// U18 INVERSE: LXC is FIAT — its gates survive the master kill (env-true → on),
	// so a fiat-SaaS deployment can still meter/gate paid LXC credit economy-off.
	if !cfg.LXCGatingEnabled || !cfg.LXCShadowSpendEnabled {
		t.Errorf("LXC gates must SURVIVE the master kill (fiat): gating=%v shadow=%v, want both true",
			cfg.LXCGatingEnabled, cfg.LXCShadowSpendEnabled)
	}
	for name, on := range checks {
		if on {
			t.Errorf("gate %s is ON with the master off (force-off failed)", name)
		}
	}
}

// TestEconomyKillSwitch_DefaultPreservesGates — nothing set ⇒ master defaults
// true, so the force-off does NOT fire and each gate keeps its own (default-off)
// value. This is the zero-change guarantee.
func TestEconomyKillSwitch_DefaultPreservesGates(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_PATTERN_MINING_ENABLED", "true") // an explicitly-on gate
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EconomyEnabled {
		t.Fatal("EconomyEnabled must default to true")
	}
	if !cfg.PatternMiningEnabled {
		t.Fatal("an explicitly-on gate must survive when the master defaults on (zero change)")
	}
	if cfg.TrustfulComputeMintEnabled {
		t.Fatal("U6 Sybil floor: TrustfulComputeMint must now default FALSE (an unprotected mint path is opt-in, not on-by-accident)")
	}
}

// TestEconomyKillSwitch_RouteGuard404 — the econ chokepoint: when off, an economy
// route is never registered ⇒ chi-native 404; when on it serves. Behavioral.
func TestEconomyKillSwitch_RouteGuard404(t *testing.T) {
	h := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	hit := func(on bool) int {
		r := chi.NewRouter()
		econReg{on: on}.get(r, "/v1/workspaces/{wsID}/tokens/balance", h)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws/tokens/balance", nil))
		return rec.Code
	}
	if got := hit(false); got != http.StatusNotFound {
		t.Errorf("master OFF: economy route should 404 (unregistered), got %d", got)
	}
	if got := hit(true); got != http.StatusOK {
		t.Errorf("master ON: economy route should serve 200, got %d", got)
	}
}

// economyManifest — the prefixes that define the economy surface. Adding a new
// economy route? Add it here AND register it through econ.{get,post,del}.
var economyManifest = []string{
	`/v1/tokens/rates`, `/v1/economy/`, `/v1/marketplace`, `/v1/insights/routing`, `/v1/oracle/stats`,
	// U18: only lxc/convert (burns LENS) is economy; lxc/balance is fiat (bare).
	`/v1/workspaces/{wsID}/tokens`, `/v1/workspaces/{wsID}/lxc/convert`, `/v1/workspaces/{wsID}/pattern-mining`,
	`/v1/workspaces/{wsID}/annotate/stake`, `/v1/workspaces/{wsID}/povi/receipts`,
	`/v1/povi/`, `/v1/admin/conversion-rate/approve`, `/v1/admin/pool-royalty/adjudicate`,
	`/v1/admin/distill/attribution`, `/dashboard/tokens`, `/dashboard/oracle`, `/dashboard/economy`,
}

// bareReg matches a BARE (non-econ) chi registration: router.Verb("/path".
var bareReg = regexp.MustCompile(`\b(?:authed|pub|r)\.(?:Get|Post|Delete)\("([^"]+)"`)

// TestEconomyKillSwitch_ManifestCoverage — the forgotten-gate tripwire. Every
// economy-manifest route in main.go must be registered through econ.{...}; a bare
// router.Verb("/v1/economy-path" fails the build. (distill/preview and
// dashboard/nodes are NOT economy and must stay bare.)
func TestEconomyKillSwitch_ManifestCoverage(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	for _, m := range bareReg.FindAllStringSubmatch(string(src), -1) {
		path := m[1]
		if isEconomyPath(path) {
			t.Errorf("economy route %q is registered BARE (not via econ.) — add the guard", path)
		}
	}
	// Negative controls: these economy-adjacent routes are deliberately NOT economy.
	// /lxc/balance is FIAT (U18) — must NOT be classified economy; /lxc/convert IS.
	for _, keep := range []string{
		"/v1/admin/distill/preview", "/dashboard/nodes", "/v1/workspaces/{wsID}/lxc/balance",
		// U18b billing is FIAT — never economy (gated by billReg/BillingEnabled, not econ).
		"/v1/billing/webhook", "/v1/workspaces/{wsID}/billing/checkout", "/v1/admin/billing/purchases",
	} {
		if isEconomyPath(keep) {
			t.Errorf("%q wrongly classified as economy", keep)
		}
	}
	if !isEconomyPath("/v1/workspaces/{wsID}/lxc/convert") {
		t.Error("/lxc/convert must stay economy (it burns LENS)")
	}
}

func isEconomyPath(path string) bool {
	for _, p := range economyManifest {
		// "/v1/admin/distill/attribution" is exact (don't catch /preview); the
		// others are prefixes.
		if p == "/v1/admin/distill/attribution" {
			if path == p {
				return true
			}
			continue
		}
		if len(path) >= len(p) && path[:len(p)] == p {
			return true
		}
	}
	return false
}

// TestEconomyKillSwitch_WorkersGuarded — the two economy background workers must
// start inside an `if cfg.EconomyEnabled` block.
func TestEconomyKillSwitch_WorkersGuarded(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	lines := strings.Split(string(src), "\n")
	for _, worker := range []string{`"pool-royalty-finalize"`, `"povi-challenge"`} {
		idx := -1
		for i, ln := range lines {
			if strings.Contains(ln, "haComps.leader.Run") && strings.Contains(ln, worker) {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Errorf("worker %s not found", worker)
			continue
		}
		guarded := false
		for j := idx; j >= 0 && j > idx-4; j-- {
			if strings.Contains(lines[j], "if cfg.EconomyEnabled {") {
				guarded = true
				break
			}
		}
		if !guarded {
			t.Errorf("worker %s is not gated on cfg.EconomyEnabled", worker)
		}
	}
}

// TestEconomyKillSwitch_LXCWiringUnconditional — the U18 fiat invariant at the
// INSTALL site (the structural complement to the behavioral pin in internal/proxy:
// TestEconomyKillSwitch_LXCGateWorksFiatMode). The LXC gate + shadow hooks must be
// wired UNCONDITIONALLY — like the fiat routes — NOT inside an `if cfg.EconomyEnabled`
// block; else the master kill would silently disable paid-credit gating, the exact
// bug U18a exists to prevent. This is the precise INVERSE of WorkersGuarded: those
// two workers MUST be econ-guarded; these two hooks must NOT be. "Unconditional" ⇒
// a top-level run() statement ⇒ exactly one leading tab; nesting in any block
// indents to >=2 tabs. Fails if a hook is deleted (never installed) OR moved under
// a guard.
func TestEconomyKillSwitch_LXCWiringUnconditional(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	lines := strings.Split(string(src), "\n")
	for _, hook := range []string{"p.SetLXCGate(", "p.SetLXCSpendSink("} {
		present, unconditional := false, false
		for _, ln := range lines {
			if strings.Contains(ln, hook) {
				present = true
				if strings.HasPrefix(ln, "\t"+hook) { // exactly one leading tab
					unconditional = true
				}
			}
		}
		switch {
		case !present:
			t.Errorf("LXC hook %q not installed in main.go — fiat gating/shadow would never fire", hook)
		case !unconditional:
			t.Errorf("LXC hook %q is indented inside a block (>=2 tabs) — it must be an unconditional top-level run() wiring (fiat survives the master kill)", hook)
		}
	}
}

// TestEconomyKillSwitch_NoDirectEnvReads — a direct os.Getenv/os.LookupEnv of an
// economy gate ANYWHERE outside internal/config bypasses the master switch (the
// force-off only rewrites cfg fields). Walk the repo and assert none exist.
func TestEconomyKillSwitch_NoDirectEnvReads(t *testing.T) {
	var offenders []string
	err := filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if strings.Contains(filepath.ToSlash(p), "/internal/config/") {
			return nil // config.Load is the ONE legitimate reader (it owns the force-off)
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := string(src)
		for _, env := range economyGateEnv {
			if strings.Contains(s, `os.Getenv("`+env+`")`) || strings.Contains(s, `os.LookupEnv("`+env+`")`) {
				offenders = append(offenders, p+" reads "+env)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, o := range offenders {
		t.Errorf("MASTER-SWITCH BYPASS — direct economy-gate env read: %s", o)
	}
}

// TestEconomyKillSwitch_DashboardHidesEconomy — master off ⇒ the rendered HTML
// has no economy nav links and no ECON-marked content, but KEEPS the fiat ROI
// panel; master on ⇒ economy nav present, markers removed.
func TestEconomyKillSwitch_DashboardHidesEconomy(t *testing.T) {
	render := func(on bool) string {
		h := dashboard.New("t", on)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
		return rec.Body.String()
	}

	off := render(false)
	if strings.Contains(off, "Tokens &amp; Mining") || strings.Contains(off, ">Economy</a>") {
		t.Error("master off: economy nav links must be stripped")
	}
	if strings.Contains(off, "{{ECON}}") {
		t.Error("master off: no ECON marker comments should remain")
	}
	if !strings.Contains(off, `id="roi-panel"`) {
		t.Error("master off: the fiat ROI panel must still be present")
	}
	if !strings.Contains(off, `id="lxc-balance-panel"`) {
		t.Error("master off: the fiat LXC credit-balance panel must still be present (#182)")
	}

	on := render(true)
	if !strings.Contains(on, "Tokens &amp; Mining") || !strings.Contains(on, ">Economy</a>") {
		t.Error("master on: economy nav links must be present")
	}
	if strings.Contains(on, "{{ECON}}") {
		t.Error("master on: marker comments must be removed (content kept)")
	}
	if !strings.Contains(on, `id="roi-panel"`) {
		t.Error("master on: the ROI panel must be present")
	}
	if !strings.Contains(on, `id="lxc-balance-panel"`) {
		t.Error("master on: the fiat LXC credit-balance panel must be present (#182)")
	}
}

// TestEconomyKillSwitch_BillingFiatIndependent — U18b/U3 interplay at the ROUTE
// layer: billing is FIAT, gated by billReg (cfg.BillingEnabled), INDEPENDENT of
// the economy master. With the economy OFF and billing ON (the fiat-SaaS shape),
// an economy route 404s while the billing routes SERVE; with billing OFF the
// billing routes 404. (The behavioral half — webhook credits, lxc/balance
// reflects it with the economy off — is pinned against real PG in
// internal/billing; billing never reads cfg.EconomyEnabled, so it is economy-
// independent by construction.)
func TestEconomyKillSwitch_BillingFiatIndependent(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	hit := func(r chi.Router, method, path string) int {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec.Code
	}

	// economy master OFF, billing ON.
	r := chi.NewRouter()
	econReg{on: false}.get(r, "/v1/workspaces/{wsID}/lxc/convert", ok) // economy (burns LENS)
	bill := billReg{on: true}
	bill.post(r, "/v1/billing/webhook", ok)
	bill.post(r, "/v1/workspaces/{wsID}/billing/checkout", ok)

	if got := hit(r, http.MethodGet, "/v1/workspaces/ws/lxc/convert"); got != http.StatusNotFound {
		t.Errorf("economy route must 404 with the master off; got %d", got)
	}
	if got := hit(r, http.MethodPost, "/v1/billing/webhook"); got != http.StatusOK {
		t.Errorf("billing webhook must SERVE with billing on while economy off; got %d", got)
	}
	if got := hit(r, http.MethodPost, "/v1/workspaces/ws/billing/checkout"); got != http.StatusOK {
		t.Errorf("billing checkout must SERVE with billing on while economy off; got %d", got)
	}

	// billing OFF ⇒ unregistered ⇒ 404.
	rOff := chi.NewRouter()
	billReg{on: false}.post(rOff, "/v1/billing/webhook", ok)
	if got := hit(rOff, http.MethodPost, "/v1/billing/webhook"); got != http.StatusNotFound {
		t.Errorf("billing OFF ⇒ webhook 404 (unregistered); got %d", got)
	}
}

// TestBillingSecrets_ReadOnlyInConfig — the Stripe secrets (and the billing
// switch) must be read ONLY in internal/config; a direct os.Getenv/os.LookupEnv
// anywhere else risks logging a key or bypassing the enabled-without-keys startup
// validation. Mirrors NoDirectEnvReads for the billing surface.
func TestBillingSecrets_ReadOnlyInConfig(t *testing.T) {
	billingEnv := []string{"LENS_STRIPE_SECRET_KEY", "LENS_STRIPE_WEBHOOK_SECRET", "LENS_BILLING_ENABLED"}
	var offenders []string
	err := filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if strings.Contains(filepath.ToSlash(p), "/internal/config/") {
			return nil // config.Load is the ONE legitimate reader
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := string(src)
		for _, env := range billingEnv {
			if strings.Contains(s, `os.Getenv("`+env+`")`) || strings.Contains(s, `os.LookupEnv("`+env+`")`) {
				offenders = append(offenders, p+" reads "+env)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, o := range offenders {
		t.Errorf("billing secret/switch read outside internal/config: %s", o)
	}
}
