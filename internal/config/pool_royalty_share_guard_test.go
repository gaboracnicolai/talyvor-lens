package config

// pool_royalty_share_guard_test.go — the royalty share is a SECURITY parameter.
//
// Per-identity workspaces (POST /v1/provision) make Sybil self-dealing physically possible for
// the first time: one person can hold two workspaces and have A's cached answers serve B. Neither
// identity guard stops it. workspace_owner_links has no writer at all, and the card-fingerprint
// half catches only the lazy one-card operator — two virtual cards defeat it in minutes.
//
// What actually holds is arithmetic. poolroyalty/anchor.go clamps a royalty's dollar worth to
// <= AvoidedCOGSUSD, and the mint is funded by the CONSUMER's settled charge, so a Sybil pair
// spends C to earn s*C and loses (1-s)*C on every cycle. At the 0.5 default that is a 50% loss —
// the "farm" is a way to pay two dollars to receive one.
//
// That protection is entirely a function of s < 1. At s = 1.0 it degrades to break-even and the
// only economic control evaporates; above 1.0 the clamp still binds, but the intent is plainly
// wrong. A convention nobody knows about is not a control, so Load() refuses to boot there and
// says why.
//
// Strictly additive: every configuration that boots today keeps booting except s >= 1.0, which
// is not a value anyone can currently be relying on for safety.

import (
	"strings"
	"testing"
)

func TestLoad_PoolRoyaltyShare_RefusesUnityAndAbove(t *testing.T) {
	t.Run("share 1.0 is refused", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_POOL_ROYALTY_SHARE", "1.0")
		_, err := Load()
		if err == nil {
			t.Fatal("LENS_POOL_ROYALTY_SHARE=1.0 must be refused at load — at s=1 a Sybil pair breaks even instead of losing (1-s)*C per cycle, removing the only thing that makes per-identity workspaces safe while workspace_owner_links has no writer")
		}
		if !strings.Contains(err.Error(), "LENS_POOL_ROYALTY_SHARE") {
			t.Errorf("refusal must name the offending knob; got: %v", err)
		}
		// It must explain WHY, not merely restate the bound — the next person to hit this is
		// mid-incident and needs the reason, not the range.
		low := strings.ToLower(err.Error())
		if !strings.Contains(low, "self-deal") && !strings.Contains(low, "sybil") {
			t.Errorf("refusal must explain the self-dealing reason, not just the range; got: %v", err)
		}
	})

	t.Run("share above 1.0 is refused", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_POOL_ROYALTY_SHARE", "1.5")
		if _, err := Load(); err == nil {
			t.Fatal("LENS_POOL_ROYALTY_SHARE=1.5 must be refused at load")
		}
	})

	t.Run("safe: the 0.5 default boots and is unchanged", func(t *testing.T) {
		setRequiredEnv(t)
		c, err := Load()
		if err != nil {
			t.Fatalf("default must boot: %v", err)
		}
		if c.PoolRoyaltyShare != 0.5 {
			t.Errorf("PoolRoyaltyShare = %v, want the 0.5 default", c.PoolRoyaltyShare)
		}
	})

	t.Run("safe: a share just under 1.0 still boots", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_POOL_ROYALTY_SHARE", "0.99")
		c, err := Load()
		if err != nil {
			t.Fatalf("0.99 keeps the loss-making property (1%% per cycle) and must boot: %v", err)
		}
		if c.PoolRoyaltyShare != 0.99 {
			t.Errorf("PoolRoyaltyShare = %v, want 0.99", c.PoolRoyaltyShare)
		}
	})

	t.Run("safe: zero share boots — nothing is minted at all", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_POOL_ROYALTY_SHARE", "0")
		if _, err := Load(); err != nil {
			t.Fatalf("s=0 mints nothing and is trivially safe; must boot: %v", err)
		}
	})

	t.Run("still rejects NaN and negatives", func(t *testing.T) {
		for _, v := range []string{"NaN", "-0.1", "not-a-number"} {
			setRequiredEnv(t)
			t.Setenv("LENS_POOL_ROYALTY_SHARE", v)
			if _, err := Load(); err == nil {
				t.Errorf("LENS_POOL_ROYALTY_SHARE=%q must still be refused", v)
			}
		}
	})
}
