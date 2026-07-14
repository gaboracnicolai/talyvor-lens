package config

import (
	"os"
	"testing"
)

// After the full Keel default-on sweep, the shipped DEFAULTS must be: every Keel flag ON, and every threshold
// STILL its documented N3 placeholder. Flipping flags must not have quietly moved a threshold. This is the
// proof the sweep changed only postures, never money-grade values.
func TestKeelDefaults_FlagsOn_ThresholdsUnchangedPlaceholders(t *testing.T) {
	setRequiredEnv(t)
	// Clear every Keel flag/threshold env so we read pure shipped defaults.
	for _, k := range []string{
		"LENS_KEEL_ENABLED", "LENS_KEEL_HARDENED_ENABLED", "LENS_KEEL_ROYALTY_HAIRCUT_ENABLED",
		"LENS_ROUTING_DECISION_CAPTURE_ENABLED", "LENS_KEEL_DEVIATION_SIGMA", "LENS_KEEL_MONEY_COHORT_FLOOR",
		"LENS_KEEL_MIN_SAMPLES", "LENS_KEEL_PERSISTENCE_WINDOWS",
	} {
		old, had := os.LookupEnv(k)
		os.Unsetenv(k)
		if had {
			kk, vv := k, old
			t.Cleanup(func() { os.Setenv(kk, vv) })
		}
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Flags: the full default-on sweep.
	if !cfg.KeelEnabled {
		t.Error("KeelEnabled must default TRUE")
	}
	if !cfg.KeelHardenedEnabled {
		t.Error("KeelHardenedEnabled must default TRUE (closed-test — this run's flip)")
	}
	if !cfg.KeelRoyaltyHaircutEnabled {
		t.Error("KeelRoyaltyHaircutEnabled must default TRUE")
	}
	if !cfg.RoutingDecisionCaptureEnabled {
		t.Error("RoutingDecisionCaptureEnabled must default TRUE")
	}

	// Thresholds: UNCHANGED N3 placeholders (no calibration happened — these must not move on a flag flip).
	if cfg.KeelDeviationSigma != 2.0 {
		t.Errorf("KeelDeviationSigma = %v, want 2.0 (placeholder)", cfg.KeelDeviationSigma)
	}
	if cfg.KeelMoneyCohortFloor != 10 {
		t.Errorf("KeelMoneyCohortFloor = %d, want 10 (placeholder)", cfg.KeelMoneyCohortFloor)
	}
	if cfg.KeelMinSamples != 30 {
		t.Errorf("KeelMinSamples = %d, want 30 (placeholder)", cfg.KeelMinSamples)
	}
	if cfg.KeelPersistenceWindows != 3 {
		t.Errorf("KeelPersistenceWindows = %d, want 3 (placeholder)", cfg.KeelPersistenceWindows)
	}
}
