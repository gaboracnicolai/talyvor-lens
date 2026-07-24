package config

import "testing"

// TestLoad_AnnotationMintingGate pins the GA activation decision for the annotation
// mint: it credits SPENDABLE-IMMEDIATE LENS (CreditTx, not a held→examined→settled
// path like pattern), so it must NOT be armed by the bare EconomyEnabled switch —
// it is an explicit opt-in. Meanwhile the two sound, funded mints (pattern held +
// pool royalty) stay ON by default.
func TestLoad_AnnotationMintingGate(t *testing.T) {
	t.Run("defaults OFF — spendable-immediate mint is opt-in, not on-by-EconomyEnabled", func(t *testing.T) {
		setRequiredEnv(t)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.AnnotationMintingEnabled {
			t.Error("AnnotationMintingEnabled must default FALSE: annotation credits spendable-immediate LENS with no holdback/examiner, so it must be an explicit opt-in, not armed by the bare economy switch")
		}
		// Regression: the funded/sound mints must stay default-ON (I only gated annotation).
		if !c.PoolRoyaltyMintingEnabled {
			t.Error("PoolRoyaltyMinting (consumer-funded via #351) must stay default-ON")
		}
		if !c.PatternEarningEnabled {
			t.Error("PatternEarning (protocol-issuance, held+examined) must stay default-ON")
		}
	})
	t.Run("parsed when explicitly enabled", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_ANNOTATION_MINTING_ENABLED", "true")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !c.AnnotationMintingEnabled {
			t.Error("LENS_ANNOTATION_MINTING_ENABLED=true must enable the annotation mint")
		}
	})
	t.Run("force-off'd by the economy kill switch", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_ANNOTATION_MINTING_ENABLED", "true")
		t.Setenv("LENS_ECONOMY_ENABLED", "false")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.AnnotationMintingEnabled {
			t.Error("EconomyEnabled=false must force AnnotationMintingEnabled off (it mints LENS)")
		}
	})
}
