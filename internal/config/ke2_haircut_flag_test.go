package config

import (
	"os"
	"testing"
)

// KE-2: the drift haircut flips to DEFAULT-ON (closed-test). Proves the default actually took effect — with no
// env set the flag is TRUE — and that the env can still force it OFF (then the mint path takes no haircut).
func TestKeelRoyaltyHaircut_DefaultsOn(t *testing.T) {
	setRequiredEnv(t)
	old, had := os.LookupEnv("LENS_KEEL_ROYALTY_HAIRCUT_ENABLED")
	os.Unsetenv("LENS_KEEL_ROYALTY_HAIRCUT_ENABLED")
	t.Cleanup(func() {
		if had {
			os.Setenv("LENS_KEEL_ROYALTY_HAIRCUT_ENABLED", old)
		}
	})

	// BEFORE (no env): must be ON out of the box.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.KeelRoyaltyHaircutEnabled {
		t.Fatal("KeelRoyaltyHaircutEnabled must DEFAULT-ON (closed-test) — no env set should yield true")
	}

	// AFTER (env=false): must force OFF.
	t.Setenv("LENS_KEEL_ROYALTY_HAIRCUT_ENABLED", "false")
	cfg2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.KeelRoyaltyHaircutEnabled {
		t.Fatal("LENS_KEEL_ROYALTY_HAIRCUT_ENABLED=false must force the haircut OFF")
	}
}
