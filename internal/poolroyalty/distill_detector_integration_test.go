package poolroyalty

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PR2 — distill anti-gaming detectors (observe/flag, read-only). Proofs on real PG:
// swarm/collusion flagged, clean traffic not flagged, no-rows → empty (the
// minting-off / "default-off silent" guarantee). Reuses distillMintHarness.

func distillDetectorThresholds() DetectorThresholds {
	return DetectorThresholds{
		VolumeMinMints:    4,   // a content reused by >= 4 distinct requesters → swarm flag
		BilateralMinFrac:  0.9, // a pair carrying >= 90% of each side's flow
		BilateralMinMints: 3,   // ...AND >= 3 mints
		// VolumeMaxRequesters / Similarity* unused for distill (see distill_detector.go).
	}
}

// seedMint inserts one distill_royalty_mints row (the detector's input). request_id
// is the real once-per-relationship key so duplicates collapse like production.
func seedMint(t *testing.T, pool *pgxpool.Pool, owner, requester, content string) {
	t.Helper()
	rid := SHA256Hex([]byte(owner + ":" + requester + ":" + content))
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO distill_royalty_mints
		   (request_id, contributor_workspace_id, requester_workspace_id, content_hash, avoided_cogs_usd, minted_amount, status)
		 VALUES ($1,$2,$3,$4,2.0,1.0,'final') ON CONFLICT (request_id) DO NOTHING`,
		rid, owner, requester, content); err != nil {
		t.Fatalf("seed mint: %v", err)
	}
}

// (PR2.a) Volume SWARM flagged: one owner's content reused by 5 distinct requesters
// (the sock-puppet swarm) → flagged; a content reused by only 2 → not flagged.
func TestDistillDetector_VolumeSwarm_Flagged_Integration(t *testing.T) {
	pool, _ := distillMintHarness(t)
	ctx := context.Background()
	for _, req := range []string{"r1", "r2", "r3", "r4", "r5"} {
		seedMint(t, pool, "wsA", req, "hotdoc") // 5 distinct requesters on one content
	}
	seedMint(t, pool, "wsA", "r1", "cleandoc")
	seedMint(t, pool, "wsA", "r2", "cleandoc") // only 2

	flags, err := NewDistillDetectorReader(pool, distillDetectorThresholds()).VolumeConcentration(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	hot, clean := false, false
	for _, f := range flags {
		if f.ContentHash == "hotdoc" && f.Flagged {
			hot = true
		}
		if f.ContentHash == "cleandoc" && f.Flagged {
			clean = true
		}
	}
	if !hot {
		t.Error("hotdoc (5 distinct requesters) must be flagged as a swarm")
	}
	if clean {
		t.Error("cleandoc (2 requesters) must NOT be flagged")
	}
}

// (PR2.b) Bilateral COLLUSION flagged: a tight (owner, requester) pair carrying all
// of each side's flow → flagged; an owner spread across distinct requesters → not.
func TestDistillDetector_BilateralCollusion_Flagged_Integration(t *testing.T) {
	pool, _ := distillMintHarness(t)
	ctx := context.Background()
	for _, c := range []string{"d1", "d2", "d3", "d4"} {
		seedMint(t, pool, "wsA", "wsB", c) // wsA↔wsB: 4 mints, all of each side's flow
	}
	seedMint(t, pool, "wsC", "x1", "e1") // wsC spread across 3 distinct requesters
	seedMint(t, pool, "wsC", "x2", "e2")
	seedMint(t, pool, "wsC", "x3", "e3")

	flags, err := NewDistillDetectorReader(pool, distillDetectorThresholds()).BilateralConcentration(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ab, cx := false, false
	for _, f := range flags {
		if f.ContributorWorkspace == "wsA" && f.RequesterWorkspace == "wsB" && f.Flagged {
			ab = true
		}
		if f.ContributorWorkspace == "wsC" && f.Flagged {
			cx = true
		}
	}
	if !ab {
		t.Error("(wsA,wsB) tight pair (4 mints, frac 1.0 each) must be flagged")
	}
	if cx {
		t.Error("wsC spread across distinct requesters (frac 0.33) must NOT be flagged")
	}
}

// (PR2.c) Default-off / minting-off → SILENT: no distill_royalty_mints rows → every
// detector returns empty. (This is the "inert by construction" guarantee — with the
// mint flag off there are ~no rows.)
func TestDistillDetector_NoRows_Silent_Integration(t *testing.T) {
	pool, _ := distillMintHarness(t)
	ctx := context.Background()
	r := NewDistillDetectorReader(pool, distillDetectorThresholds())
	if v, err := r.VolumeConcentration(ctx, time.Hour); err != nil || len(v) != 0 {
		t.Fatalf("no rows → empty volume; got %d err=%v", len(v), err)
	}
	if b, err := r.BilateralConcentration(ctx, time.Hour); err != nil || len(b) != 0 {
		t.Fatalf("no rows → empty bilateral; got %d err=%v", len(b), err)
	}
}

// TestDistillDetector_NilInert — a nil reader is a no-op (mirrors the cache detector;
// the shared read-only detectorDB seam gives the type-level never-auto-act guarantee).
func TestDistillDetector_NilInert(t *testing.T) {
	var r *DistillDetectorReader
	if v, err := r.VolumeConcentration(context.Background(), time.Hour); err != nil || v != nil {
		t.Fatalf("nil reader volume must be (nil,nil), got %v %v", v, err)
	}
	if b, err := r.BilateralConcentration(context.Background(), time.Hour); err != nil || b != nil {
		t.Fatalf("nil reader bilateral must be (nil,nil), got %v %v", b, err)
	}
}
