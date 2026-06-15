package main

import (
	"os"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/povi"
)

// TestMintTypes_CoversPoVIReceipt pins the cycle-free string literal in mining's
// mintTypes set to povi's ACTUAL constant (mining is imported by povi, so the
// gate set can't reference it directly). If the constant is ever renamed, this
// cross-package test fails rather than silently dropping PoVI from the gate.
func TestMintTypes_CoversPoVIReceipt(t *testing.T) {
	if !mining.IsMintType(povi.TypeReceiptMineProvisional) {
		t.Fatalf("the verified-to-earn gate must cover povi.TypeReceiptMineProvisional (%q)", povi.TypeReceiptMineProvisional)
	}
}

// TestU6_TrustfulComputeMintDefaultsFalse — PIECE 2: the unprotected legacy
// compute mint (no receipt, caller-asserted tokens, no idempotency) is now
// opt-IN, not on-by-accident.
func TestU6_TrustfulComputeMintDefaultsFalse(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TrustfulComputeMintEnabled {
		t.Fatal("U6: TrustfulComputeMintEnabled must default FALSE (an unprotected mint path is opt-in)")
	}
}

// TestU6_MintVerifierWiredUnconditional — the Sybil floor must be wired
// UNCONDITIONALLY: a safety restriction must NOT be liftable by the economy
// master kill (the precise analogue of the LXC-fiat unconditional-wiring
// invariant, TestEconomyKillSwitch_LXCWiringUnconditional). The SetMintVerifier
// call must be a top-level run() statement (exactly one leading tab), never
// nested inside an `if cfg.EconomyEnabled` block.
func TestU6_MintVerifierWiredUnconditional(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	present, unconditional := false, false
	for _, ln := range strings.Split(string(src), "\n") {
		if strings.Contains(ln, "SetMintVerifier(") {
			present = true
			if strings.HasPrefix(ln, "\ttokenLedger.SetMintVerifier(") { // exactly one leading tab
				unconditional = true
			}
		}
	}
	if !present {
		t.Fatal("SetMintVerifier not wired in main.go — the Sybil floor would never enforce")
	}
	if !unconditional {
		t.Fatal("SetMintVerifier must be an unconditional top-level run() wiring (one leading tab) — a safety restriction must survive the economy master kill")
	}
}
