package poolroyalty

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PR4 — the distill reuse-royalty margin view (margin_usd = avoided_cogs_usd −
// minted_amount over distill_royalty_mints). Realized margin counts FINAL rows only.
// Reuses distillMintHarness/seedBasis/verifyWorkspace.

func createDistillMarginView(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `CREATE OR REPLACE VIEW distill_royalty_margin AS
SELECT request_id, requester_workspace_id, contributor_workspace_id, content_hash,
       avoided_cogs_usd, minted_amount, avoided_cogs_usd - minted_amount AS margin_usd,
       status, created_at FROM distill_royalty_mints`); err != nil {
		t.Fatalf("create distill_royalty_margin view: %v", err)
	}
	// Drop the view at test end — else the NEXT test's harness DROP TABLE
	// distill_royalty_mints fails (a view depends on it, SQLSTATE 2BP01).
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP VIEW IF EXISTS distill_royalty_margin`) })
}

// (PR4) Realized margin = FINAL rows only; the identity margin == avoided − minted holds;
// a HELD mint is excluded; the breakdown groups by content_hash; a cache-only dimension is rejected.
func TestDistillMargin_RealizedMargin_FinalOnly_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	createDistillMarginView(t, pool)
	verifyWorkspace(t, pool, "wsA")

	// h1, h2 → mint + FINALIZE (these are the realized margin).
	seedBasis(t, pool, "wsA", "wsB", "h1", 4.0)
	seedBasis(t, pool, "wsA", "wsC", "h2", 4.0)
	mFinal := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })
	mFinal.SetHoldbackWindow(time.Nanosecond)
	if n, err := mFinal.RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("mint h1,h2: minted %d err=%v, want 2", n, err)
	}
	sw := NewFinalizeSweeper(pool, ledger, "distill_royalty_mints")
	if _, err := sw.RunOnce(ctx); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	// h3 → HELD (default 72h holdback) — must be EXCLUDED from realized margin.
	seedBasis(t, pool, "wsA", "wsD", "h3", 4.0)
	mHeld := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })
	if n, err := mHeld.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("mint h3 (held): minted %d err=%v, want 1", n, err)
	}

	r := NewDistillMarginReader(pool)
	s, err := r.MarginSummary(ctx, time.Time{})
	if err != nil {
		t.Fatalf("margin summary: %v", err)
	}
	// 2 FINAL mints: avoided 8.0, minted 4.0 (2 × 0.5 × 4.0), margin 4.0. h3 (held) excluded.
	if s.Mints != 2 || s.AvoidedCOGSUSD != 8.0 || s.MintedLENS != 4.0 || s.MarginUSD != 4.0 {
		t.Fatalf("summary mints=%d avoided=%v minted=%v margin=%v; want 2/8/4/4 (final only)",
			s.Mints, s.AvoidedCOGSUSD, s.MintedLENS, s.MarginUSD)
	}
	// The margin identity: margin == avoided − minted.
	if s.MarginUSD != s.AvoidedCOGSUSD-s.MintedLENS {
		t.Fatalf("margin identity broken: %v != %v − %v", s.MarginUSD, s.AvoidedCOGSUSD, s.MintedLENS)
	}

	// Breakdown by content_hash → 2 buckets (h1, h2).
	by, err := r.MarginBy(ctx, "content_hash", time.Time{})
	if err != nil {
		t.Fatalf("margin by content_hash: %v", err)
	}
	if len(by) != 2 {
		t.Fatalf("by content_hash: %d buckets, want 2", len(by))
	}

	// 'layer' is a CACHE dimension, not distill → rejected before any query runs.
	if _, err := r.MarginBy(ctx, "layer", time.Time{}); err == nil {
		t.Fatal("layer must be rejected for the distill margin allow-list")
	}
}

func TestDistillMargin_NilInert(t *testing.T) {
	var r *DistillMarginReader
	if s, err := r.MarginSummary(context.Background(), time.Time{}); err != nil || s != (MarginSummaryRow{}) {
		t.Fatalf("nil reader summary must be (zero,nil), got %v %v", s, err)
	}
	if b, err := r.MarginBy(context.Background(), "content_hash", time.Time{}); err != nil || b != nil {
		t.Fatalf("nil reader by must be (nil,nil), got %v %v", b, err)
	}
}
