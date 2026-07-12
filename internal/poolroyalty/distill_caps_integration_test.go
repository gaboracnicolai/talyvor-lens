package poolroyalty

import (
	"context"
	"testing"
	"time"
)

// PR1 — distill per-pair / per-content mint caps (mirroring the cache minter's
// per-pair/per-entry caps over distill_royalty_mints). Each asserts on the LEDGER
// + the claim table. Reuses distillMintHarness/seedBasis/verifyWorkspace/
// mintRowCount/balances from distill_minter_integration_test.go.

// (PR1.a) Per-pair cap DENIES over-cap: 3 relationships for one (owner, requester)
// pair, cap=2 → exactly 2 mint, the 3rd is a deflationary no-op (no claim row, no
// credit). The owner's held balance is 2×0.5×2.0, not 3×.
func TestDistillMint_PerPairCap_DeniesOverCap_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "wsA")
	seedBasis(t, pool, "wsA", "wsB", "h1", 2.0)
	seedBasis(t, pool, "wsA", "wsB", "h2", 2.0)
	seedBasis(t, pool, "wsA", "wsB", "h3", 2.0)

	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetCap(2, time.Hour) // per-pair cap = 2

	n, err := m.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("per-pair cap=2 over 3 relationships: minted %d, want 2 (3rd capped)", n)
	}
	if c := mintRowCount(t, pool, "wsA"); c != 2 {
		t.Fatalf("cap: %d claim rows, want 2 (the 3rd must roll back — no row)", c)
	}
	if _, held := balances(t, pool, "wsA"); held != micro(2) {
		t.Fatalf("held=%v, want 2.0 (exactly 2 mints × 0.5 × 2.0; the capped 3rd added nothing)", held)
	}
}

// (PR1.b) Per-content cap DENIES over-cap: one document (content_hash) reused by 3
// distinct requesters, cap=2 → exactly 2 mint across the requesters.
func TestDistillMint_PerContentCap_DeniesOverCap_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "wsA")
	seedBasis(t, pool, "wsA", "wsB", "hot", 2.0)
	seedBasis(t, pool, "wsA", "wsC", "hot", 2.0)
	seedBasis(t, pool, "wsA", "wsD", "hot", 2.0)

	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetContentCap(2, time.Hour) // per-content cap = 2

	n, err := m.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("per-content cap=2 over 3 requesters of one doc: minted %d, want 2", n)
	}
	if c := mintRowCount(t, pool, "wsA"); c != 2 {
		t.Fatalf("content cap: %d claim rows, want 2", c)
	}
}

// (PR1.c) Default-off (no cap set) → ALL relationships mint — today's behaviour,
// unchanged. Proves the cap is opt-in.
func TestDistillMint_CapDefaultOff_AllMint_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "wsA")
	seedBasis(t, pool, "wsA", "wsB", "h1", 2.0)
	seedBasis(t, pool, "wsA", "wsB", "h2", 2.0)
	seedBasis(t, pool, "wsA", "wsB", "h3", 2.0)

	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return true }) // NO SetCap
	n, err := m.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 3 {
		t.Fatalf("default-off (cap 0): minted %d, want 3 (all mint — unchanged from today)", n)
	}
}

// (PR1.d) Under cap → mints; and the ADVERSARIAL re-run: once at the cap, a second
// sweep mints NOTHING more (the cap holds across re-runs; revoked/held all count so
// budget is not refunded). 2 relationships under a cap of 5 → both mint; re-run → no
// new mint.
func TestDistillMint_UnderCap_Mints_AndStableOnReRun_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "wsA")
	seedBasis(t, pool, "wsA", "wsB", "h1", 2.0)
	seedBasis(t, pool, "wsA", "wsB", "h2", 2.0)

	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetCap(5, time.Hour)
	if n, err := m.RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("under cap (2 of 5): minted %d err=%v, want 2", n, err)
	}

	// Add a 3rd relationship and tighten the cap to 2 → the new one is denied,
	// and a re-run does not exceed the cap.
	seedBasis(t, pool, "wsA", "wsB", "h3", 2.0)
	m.SetCap(2, time.Hour)
	if n, err := m.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("at cap (already 2, cap now 2): minted %d err=%v, want 0", n, err)
	}
	if n, err := m.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("re-run at cap: minted %d err=%v, want 0 (cap stable, no leak)", n, err)
	}
	if c := mintRowCount(t, pool, "wsA"); c != 2 {
		t.Fatalf("cap stable: %d claim rows, want exactly 2", c)
	}
}
