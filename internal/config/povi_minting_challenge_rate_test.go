package config

// povi_minting_challenge_rate_test.go — PoVI config footgun.
//
// Provisional receipt minting (LENS_POVI_MINTING_ENABLED) credits SPENDABLE LENS
// on a node-signed receipt (povi/mint.go). A receipt is attestation + tamper-
// evidence, NOT proof of honest computation — the ONLY thing that makes minting
// on it safe is post-hoc random challenge-and-slash, whose deterrent is the
// inequality P(challenge) × slash > gain. LENS_POVI_CHALLENGE_RATE is P(challenge).
// Set it to 0 with minting on and the inequality collapses to 0 > gain: every
// fabricated receipt is minted and never challenged — the "mint on the receipt
// alone" state, reached by MISCONFIGURATION, not a code defect.
//
// Contract: Load() fails closed when minting is EFFECTIVELY on and the challenge
// rate is non-positive — the same fail-closed shape as the HA signing-key
// refusals (signing_keys_ha_test.go), with a message that says WHY.
//
// The guard keys on the EFFECTIVE POVIMintingEnabled (evaluated AFTER the economy
// kill-switch force-off), so it is strictly additive: the only configuration that
// newly fails to boot is effective-minting-on + rate<=0. Every configuration that
// boots today keeps booting — minting off at any rate, minting on with a positive
// rate (incl. the 0.1 default), and minting-INTENT on under an economy that is
// off (which force-disables minting).

import (
	"strings"
	"testing"
)

func TestLoad_POVIMinting_RequiresPositiveChallengeRate(t *testing.T) {
	t.Run("minting ON with rate 0 is refused", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_POVI_MINTING_ENABLED", "true")
		t.Setenv("LENS_POVI_CHALLENGE_RATE", "0")
		_, err := Load()
		if err == nil {
			t.Fatal("LENS_POVI_MINTING_ENABLED=true with LENS_POVI_CHALLENGE_RATE=0 must be refused at load — a zero challenge rate removes the only deterrent (challenge-and-slash) behind a spendable provisional mint, letting a node mint LENS against fabricated, never-challenged traces")
		}
		// The refusal must explain WHY and name the offending knob (not a bare error).
		if !strings.Contains(err.Error(), "LENS_POVI_CHALLENGE_RATE") {
			t.Errorf("refusal must name LENS_POVI_CHALLENGE_RATE and explain the deterrent; got: %v", err)
		}
	})

	t.Run("safe: minting OFF with rate 0 still boots", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_POVI_CHALLENGE_RATE", "0")
		// LENS_POVI_MINTING_ENABLED unset → default false → nothing mints, so a
		// zero rate is harmless.
		if _, err := Load(); err != nil {
			t.Fatalf("minting off tolerates any rate (nothing mints); must boot: %v", err)
		}
	})

	t.Run("safe: minting ON with a positive rate boots", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_POVI_MINTING_ENABLED", "true")
		t.Setenv("LENS_POVI_CHALLENGE_RATE", "0.1")
		c, err := Load()
		if err != nil {
			t.Fatalf("minting on with a positive challenge rate is the intended safe config; must boot: %v", err)
		}
		if !c.POVIMintingEnabled {
			t.Error("POVIMintingEnabled should be true")
		}
		if c.POVIChallengeRate <= 0 {
			t.Errorf("POVIChallengeRate should be positive, got %v", c.POVIChallengeRate)
		}
	})

	t.Run("safe: minting ON with the DEFAULT rate (no rate env) boots", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_POVI_MINTING_ENABLED", "true")
		// LENS_POVI_CHALLENGE_RATE unset → default 0.1 → deterrent present.
		if _, err := Load(); err != nil {
			t.Fatalf("minting on with the default (0.1) rate must boot: %v", err)
		}
	})

	// Additive-property lock. Minting-INTENT on, but the master economy switch is
	// off: the kill-switch force-disables POVIMintingEnabled, so the guard — which
	// keys on the EFFECTIVE value — must NOT fire. This config boots today and must
	// keep booting even with rate 0. Placing the guard BEFORE the force-off block
	// (keying on raw operator intent) would newly break this config; this test
	// locks the guard after force-off.
	t.Run("additive: minting-intent ON but economy OFF + rate 0 still boots", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_POVI_MINTING_ENABLED", "true")
		t.Setenv("LENS_ECONOMY_ENABLED", "false")
		t.Setenv("LENS_POVI_CHALLENGE_RATE", "0")
		c, err := Load()
		if err != nil {
			t.Fatalf("economy off force-disables minting; this config boots today and must keep booting: %v", err)
		}
		if c.POVIMintingEnabled {
			t.Error("economy off must force POVIMintingEnabled false; the guard must key on the effective value")
		}
	})
}
