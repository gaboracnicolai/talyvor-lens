package defaultsguard_test

import (
	"testing"
	"time"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/mining"
)

// THE PATTERN EARN CAP IS DECLARED THREE TIMES AND NOTHING COMPARED THEM.
//
// MEASURED 2026-08-28 (tab-k2w8, W4.40), the last of the five floors W4.36's
// reach probe flagged. This one is different from the other four, and the
// difference is the finding:
//
//	mining.DefaultPatternEarnCapPerWorkspace = 50_000   ← carries the ENTIRE
//	    justification: "50,000 events × 0.001 LENS = ~50 LENS/workspace/24h max
//	    … 10× above plausible legitimate single-workspace qualifying volume, so
//	    it never bites organic use: catastrophe-prevention, not fine-tuning."
//	config.go: c.PatternEarnCapPerWorkspace = 50000     ← the value production
//	    APPLIES: cmd/lens/main.go builds the miner and then UNCONDITIONALLY calls
//	    SetEarnCap(cfg.PatternEarnCapPerWorkspace, …), overwriting the
//	    constructor default on every boot.
//	config_test.go: `!= 50000`                          ← a third literal, which
//	    pins the second against itself.
//
// So the number carrying the attack-ceiling analysis is the one production
// throws away, and the number production keeps is justified nowhere. They are
// equal today. Nothing would have noticed if they stopped being.
//
// ⚠ THE OVERRIDE IS NOT A BUG AND IS NOT "FIXED" HERE. main.go's own comment
// says it "overrides the miner's real default from config", and the constructor
// default is the right fallback for callers that never call SetEarnCap. What
// was missing is the comparison.
//
// ⚠ THE ENFORCEMENT HALF IS ALREADY TESTED — CHECKED, NOT ASSUMED, so this file
// does not duplicate it: pattern_earn_idempotency_test.go drives the cap at 1
// and 2, and pattern_held_integration_test.go at 3 with "exactly 3 of 5
// credited — the cap must still bound the HELD credits". The cap BITES. Unlike
// the other four floors in this series, the gap here was never enforcement.
//
// ⚠ EQUALITY, NOT A BOUND, AND THE REASON IS STATED because W4.37 chose the
// opposite for the k-anonymity floor: that one had a privacy property (a
// 2-cohort mean is invertible) to derive a lower bound from, so an equality
// would have blocked a strictly-safer tightening. A cap has no such property in
// either direction — a smaller cap is not "safer", it bites organic use; a
// larger one is not "safer" either. There is nothing to derive, so the honest
// assertion is that the two declarations AGREE, whatever they say.

func TestPatternEarnCap_ConfigDefaultMatchesTheMinersConstant(t *testing.T) {
	// The required vars, mirroring internal/config's own setRequiredEnv. The cap
	// vars are blanked so what comes back is the DEFAULT and not this machine's
	// environment.
	for k, v := range map[string]string{
		"LENS_REDIS_URL":                      "redis://localhost:6379/0",
		"LENS_DATABASE_URL":                   "postgres://localhost:5432/lens",
		"LENS_NATS_URL":                       "nats://localhost:4222",
		"LENS_OPENAI_API_KEY":                 "sk-test",
		"LENS_ANTHROPIC_API_KEY":              "sk-ant-test",
		"LENS_PATTERN_EARN_CAP_PER_WORKSPACE": "",
		"LENS_PATTERN_EARN_CAP_WINDOW":        "",
	} {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	if cfg.PatternEarnCapPerWorkspace != mining.DefaultPatternEarnCapPerWorkspace {
		t.Errorf("config defaults the pattern earn cap to %d but "+
			"mining.DefaultPatternEarnCapPerWorkspace is %d.\n"+
			"cmd/lens overwrites the miner's constructor default with the config value on every "+
			"boot, so the SHIPPED cap is the config one — while the attack-ceiling analysis that "+
			"justifies the number lives on the mining constant. Change both together, and move "+
			"the reasoning if you change which one is authoritative.",
			cfg.PatternEarnCapPerWorkspace, mining.DefaultPatternEarnCapPerWorkspace)
	}

	// The window is the cap's other half: a cap of N is meaningless without the
	// period it is per. The miner's constructor uses 24h; so must config.
	if cfg.PatternEarnCapWindow != 24*time.Hour {
		t.Errorf("config defaults PatternEarnCapWindow to %v, but NewPatternMiner's constructor "+
			"uses 24h — a cap of %d events is a different cap over a different window",
			cfg.PatternEarnCapWindow, cfg.PatternEarnCapPerWorkspace)
	}
}
