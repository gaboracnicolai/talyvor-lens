package econflags_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/econflags"
)

// The point of this readout is OBSERVATION, not inference. Being shown the source and told
// "the defaults are true" is inference; this reports what the running binary's struct actually
// holds. Three properties carry that, and each is pinned here:
//
//  1. It reads the LIVE struct. A nil config reports UNREADABLE — never the defaults.
//  2. "off" and "forced off by EconomyEnabled=false" are DIFFERENT states, and the readout
//     must distinguish them. It cannot do that from the value alone: by the time anything
//     reads the struct, config.Load has already overwritten the force-off set with false, so
//     both states look identical. The distinction is derived from (EconomyEnabled, membership
//     of the force-off set).
//  3. It names the binary. A config readout from an unidentifiable binary is worth little, and
//     an UNSTAMPED binary must say so rather than report the "dev" placeholder as a commit.

func TestReport_NilConfigReportsUnreadableNotDefaults(t *testing.T) {
	snap := econflags.Report(nil, "abc1234")

	if snap.Observed {
		t.Fatal("a nil config must NOT be reported as observed")
	}
	if len(snap.Flags) != 0 {
		t.Errorf("an unreadable config must report NO flag values; got %d — reporting defaults "+
			"here is exactly the inference this endpoint exists to replace", len(snap.Flags))
	}
	if snap.Unreadable == "" {
		t.Error("it must say WHY the values could not be read")
	}
	// The known-true default must not appear anywhere in the payload.
	if strings.Contains(strings.ToLower(snap.Unreadable), "true") {
		t.Error("the unreadable reason must not smuggle a value in")
	}
}

func TestReport_ObservesLiveStructValues(t *testing.T) {
	cfg := &config.Config{EconomyEnabled: true}
	cfg.PoolRoyaltyMintingEnabled = true
	cfg.PatternEarningEnabled = false
	cfg.POVIMintingEnabled = true

	snap := econflags.Report(cfg, "abc1234")
	if !snap.Observed {
		t.Fatal("a live config must be reported as observed")
	}
	byName := map[string]econflags.Flag{}
	for _, f := range snap.Flags {
		byName[f.Name] = f
	}

	if got := byName["PoolRoyaltyMintingEnabled"]; got.State != econflags.StateOn {
		t.Errorf("PoolRoyaltyMintingEnabled: state %q, want on", got.State)
	}
	if got := byName["PatternEarningEnabled"]; got.State != econflags.StateOff {
		t.Errorf("PatternEarningEnabled: state %q, want off", got.State)
	}
	if got := byName["POVIMintingEnabled"]; got.State != econflags.StateOn {
		t.Errorf("POVIMintingEnabled: state %q, want on", got.State)
	}
	// Every flag must carry the env var that sets it, or an operator cannot act on the readout.
	for _, f := range snap.Flags {
		if f.Env == "" {
			t.Errorf("flag %q reports no env var", f.Name)
		}
	}
}

// THE DISTINCTION THAT IS THE ENTIRE POINT. With EconomyEnabled=false, config.Load overwrites
// the whole force-off set to false — so a reader looking only at values cannot tell "the
// operator turned this off" from "the master switch overrode whatever the operator asked for".
func TestReport_ForcedOffIsNotTheSameAsOff(t *testing.T) {
	cfg := &config.Config{EconomyEnabled: false}
	// Simulate the post-Load state: force-off already applied.
	cfg.POVIMintingEnabled = false
	cfg.PoolRoyaltyMintingEnabled = false
	// A flag OUTSIDE the force-off set, genuinely off on its own.
	cfg.LXCGatingEnabled = false

	snap := econflags.Report(cfg, "abc1234")
	byName := map[string]econflags.Flag{}
	for _, f := range snap.Flags {
		byName[f.Name] = f
	}

	if got := byName["POVIMintingEnabled"]; got.State != econflags.StateForcedOff {
		t.Errorf("POVIMintingEnabled with EconomyEnabled=false: state %q, want forced_off — "+
			"a reader must be able to tell this from a plain off", got.State)
	}
	if got := byName["PoolRoyaltyMintingEnabled"]; got.State != econflags.StateForcedOff {
		t.Errorf("PoolRoyaltyMintingEnabled: state %q, want forced_off", got.State)
	}
	// And a forced-off flag must explain that its configured value is NOT in effect, so an
	// operator who set the env var true understands why it is not.
	if r := byName["POVIMintingEnabled"].Note; !strings.Contains(strings.ToLower(r), "economy") {
		t.Errorf("forced_off must name EconomyEnabled as the cause, got %q", r)
	}
	// LXC is deliberately NOT force-off'd (it is fiat credit, not token economy).
	if got := byName["LXCGatingEnabled"]; got.State == econflags.StateForcedOff {
		t.Error("LXCGatingEnabled must not be reported forced_off — config.go excludes it " +
			"from the force-off block on purpose")
	}

	// With the economy ON, the same false value is a plain off.
	cfg.EconomyEnabled = true
	snap2 := econflags.Report(cfg, "abc1234")
	for _, f := range snap2.Flags {
		if f.Name == "POVIMintingEnabled" && f.State != econflags.StateOff {
			t.Errorf("with EconomyEnabled=true, a false flag is a plain off; got %q", f.State)
		}
	}
}

// Coverage: every flag in config.go's force-off block, plus the four in the default-on loop.
// Named explicitly so a flag added to config without being added here fails the build's tests
// rather than silently going unobserved.
func TestReport_CoversEveryForceOffAndDefaultOnFlag(t *testing.T) {
	want := []string{
		// the default-on loop
		"PoolRoyaltyMintingEnabled", "CachePoolableEnabled", "DistillPoolableEnabled",
		"PatternEarningEnabled",
		// the force-off block
		"PatternMiningEnabled", "PatternCaptureEnabled", "POVIMintingEnabled",
		"AnnotationMintingEnabled", "TrustfulComputeMintEnabled", "CacheSharingEnabled",
		"RoutingIntelligenceEnabled", "RoutingTierCohortsEnabled",
		"EvalContributionMintingEnabled", "RoutingPredictionMintingEnabled",
		"LatencyMintingEnabled", "ConfidentialMintingEnabled",
		// the master switch itself
		"EconomyEnabled",
	}
	snap := econflags.Report(&config.Config{EconomyEnabled: true}, "abc1234")
	have := map[string]bool{}
	for _, f := range snap.Flags {
		have[f.Name] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("flag %q is not reported — it is in config.go's force-off block or "+
				"default-on loop and must be observable", w)
		}
	}
}

func TestReport_NamesTheBinaryAndSaysWhenUnstamped(t *testing.T) {
	cfg := &config.Config{EconomyEnabled: true}

	stamped := econflags.Report(cfg, "836bf3b")
	if stamped.Binary.Commit != "836bf3b" || !stamped.Binary.Stamped {
		t.Errorf("a stamped binary must report its commit: %+v", stamped.Binary)
	}

	// "dev" is the ldflags default — an UNSTAMPED build. Reporting it as a commit would be the
	// same defect as reporting a config default as observed.
	for _, unstamped := range []string{"dev", "", "   "} {
		snap := econflags.Report(cfg, unstamped)
		if snap.Binary.Stamped {
			t.Errorf("version %q must be reported as UNSTAMPED, not as a commit", unstamped)
		}
		if snap.Binary.Note == "" {
			t.Errorf("version %q must carry a note explaining it cannot be identified", unstamped)
		}
	}
}

// The grouped view is what a human reads. It must partition the flags totally — every flag in
// exactly one group, no flag lost.
func TestReport_GroupsPartitionEveryFlag(t *testing.T) {
	cfg := &config.Config{EconomyEnabled: false}
	snap := econflags.Report(cfg, "abc1234")

	total := len(snap.Groups.On) + len(snap.Groups.Off) + len(snap.Groups.ForcedOff)
	if total != len(snap.Flags) {
		t.Errorf("groups hold %d flags but %d were reported — a flag in no group is a flag "+
			"nobody reads", total, len(snap.Flags))
	}
	seen := map[string]int{}
	for _, g := range [][]string{snap.Groups.On, snap.Groups.Off, snap.Groups.ForcedOff} {
		for _, n := range g {
			seen[n]++
		}
	}
	for n, c := range seen {
		if c != 1 {
			t.Errorf("flag %q appears in %d groups; must be exactly 1", n, c)
		}
	}
}

// ── runtime overrides ──
//
// The pooling gate holds cross-tenant pooling off when the live embedding configuration is not
// the one that passed `lens poolcheck`. cfg.CachePoolableEnabled stays TRUE the whole time, so
// without an override this endpoint reports "on" about a flag that is doing nothing — which is
// the exact defect the package doc says it exists to prevent, committed by the package itself.

func TestReport_RuntimeOverrideReportsEffectiveNotConfigured(t *testing.T) {
	cfg := &config.Config{EconomyEnabled: true, CachePoolableEnabled: true}
	const why = "no pool-safety attestation has ever been recorded for this database"

	snap := econflags.Report(cfg, "abc1234", econflags.Override{
		Name: "CachePoolableEnabled", Effective: false, Reason: why,
	})

	f := findFlag(t, snap, "CachePoolableEnabled")
	if f.Value {
		t.Fatal("the endpoint reported cross-tenant pooling as ON while the gate is holding it OFF; " +
			"an operator checking here would conclude the feature is working")
	}
	if f.State != econflags.StateForcedOffAtRuntime {
		t.Errorf("state = %q, want %q — plain \"off\" would be indistinguishable from the flag "+
			"simply not being set", f.State, econflags.StateForcedOffAtRuntime)
	}
	if f.Configured == nil || !*f.Configured {
		t.Error("the CONFIGURED value is not reported, so the readout hides that the environment " +
			"asked for pooling and is not being honoured")
	}
	if f.Note != why {
		t.Errorf("note = %q, want the gate's reason; a forced-off state with no reason is the "+
			"thing this is meant to fix", f.Note)
	}
	if !slicesContains(snap.Groups.ForcedOffAtRuntime, "CachePoolableEnabled") {
		t.Error("the flag is missing from the forced_off_at_runtime group")
	}
	if slicesContains(snap.Groups.On, "CachePoolableEnabled") {
		t.Error("the flag is ALSO listed as on; the groups must partition")
	}
}

// When the gate agrees with the config there is no override to report — a note on a flag that
// is behaving normally reads as a problem.
func TestReport_OverrideMatchingConfigIsNotReportedAsAnOverride(t *testing.T) {
	cfg := &config.Config{EconomyEnabled: true, CachePoolableEnabled: true}
	snap := econflags.Report(cfg, "abc1234", econflags.Override{
		Name: "CachePoolableEnabled", Effective: true, Reason: "",
	})
	f := findFlag(t, snap, "CachePoolableEnabled")
	if f.State != econflags.StateOn {
		t.Errorf("state = %q, want on", f.State)
	}
	if f.Configured != nil {
		t.Error("configured was reported for a flag whose effective value matches it; its presence " +
			"is supposed to BE the signal that an override is in force")
	}
	if f.Note != "" {
		t.Errorf("a normally-behaving flag carries note %q", f.Note)
	}
}

// The Handler must evaluate the supplier per request: the gate re-reads on a timer, so a value
// captured at wiring time goes stale in exactly the situation being reported.
func TestHandler_EvaluatesOverridesPerRequest(t *testing.T) {
	cfg := &config.Config{EconomyEnabled: true, CachePoolableEnabled: true}
	effective := false
	h := econflags.Handler(cfg, "abc1234", func() []econflags.Override {
		return []econflags.Override{{Name: "CachePoolableEnabled", Effective: effective, Reason: "r"}}
	})

	read := func() bool {
		rr := httptest.NewRecorder()
		h(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/economy/flags", nil))
		var snap econflags.Snapshot
		if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return findFlag(t, snap, "CachePoolableEnabled").Value
	}

	if read() {
		t.Fatal("first read reported pooling on while the gate said off")
	}
	effective = true // poolcheck runs; the gate flips WITHOUT a restart
	if !read() {
		t.Fatal("the gate opened but the endpoint still reports pooling off — the override was " +
			"captured once instead of evaluated per request, so it can never show recovery")
	}
}

func findFlag(t *testing.T, snap econflags.Snapshot, name string) econflags.Flag {
	t.Helper()
	for _, f := range snap.Flags {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("flag %q missing from the snapshot", name)
	return econflags.Flag{}
}

func slicesContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
