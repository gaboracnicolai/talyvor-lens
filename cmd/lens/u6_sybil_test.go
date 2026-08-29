package main

import (
	"fmt"
	"os"
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
// invariant, TestEconomyKillSwitch_LXCWiringUnconditional).
//
// ⚠ IT ASKED THAT OF A LEADING TAB UNTIL #520, WHICH IS A QUESTION ABOUT TEXT LAYOUT AND NOT
// ABOUT EXECUTION. Measured (~/talyvor-queue/w61-unconditional-wiring-controls-h2r7.py):
// deleting the call was CAUGHT and moving it inside `if cfg.EconomyEnabled` was CAUGHT — but
// moving it into a helper called only `if cfg.EconomyEnabled` was MISSED, and so was a helper
// that is NEVER CALLED, and so was deleting the wiring entirely and leaving the call text in a
// RAW STRING whose content line begins with one tab. A function body is indented with one tab
// like run()'s statements, so the proxy cannot tell the boot path from any other function. The
// question is now answered by reachability from run() over main.go's AST.
func TestU6_MintVerifierWiredUnconditional(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	w, err := scanWiring("main.go", src, map[string]bool{"SetMintVerifier": true})
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	assertBootReachable(t, w, "tokenLedger", "SetMintVerifier",
		"the Sybil floor would never enforce",
		"a safety restriction must survive the economy master kill")
}

// assertBootReachable is the shared assertion behind the three unconditional-wiring guards:
// the hook must be CALLED, and that call must run whenever run() runs.
func assertBootReachable(t *testing.T, w *wiringScan, receiver, method, missingWhy, liftableWhy string) {
	t.Helper()
	// Vacuity floor: a scan that resolved no reachability would report every hook as
	// conditional, and one that found no call sites would report every hook as missing.
	if len(w.reached) < 2 {
		t.Fatalf("scanWiring reached only %d functions from %s() — the scan is blind", len(w.reached), bootEntryPoint)
	}
	switch {
	case !w.present(receiver, method):
		t.Fatalf("%s.%s is not called in main.go — %s", receiver, method, missingWhy)
	case !w.unconditional(receiver, method):
		var where []string
		for _, s := range w.sites {
			if s.receiver == receiver && s.method == method {
				where = append(where, fmt.Sprintf("line %d in %s() (guarded=%v, reached-from-%s=%v)",
					s.line, s.fn, s.guarded, bootEntryPoint, w.reached[s.fn]))
			}
		}
		t.Fatalf("%s.%s is called but not on the unconditional boot path: %v — %s",
			receiver, method, where, liftableWhy)
	}
}

// TestU6PR2_WashHardeningWiredUnconditional — the PR2 rate cap and owner-linkage
// guard are SAFETY restrictions wired unconditionally — the economy kill must not lift them,
// mirroring the verifier + the LXC-fiat invariant. Same defect and same fix as #520 above.
func TestU6PR2_WashHardeningWiredUnconditional(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	w, err := scanWiring("main.go", src, map[string]bool{"SetMintRateCap": true, "SetOwnerLinkageCheck": true})
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	for _, hook := range []struct{ receiver, method string }{
		{"tokenLedger", "SetMintRateCap"},
		{"royaltyMinter", "SetOwnerLinkageCheck"},
	} {
		assertBootReachable(t, w, hook.receiver, hook.method,
			"the wash-hardening would never enforce",
			"a safety restriction must survive the economy kill")
	}
}
