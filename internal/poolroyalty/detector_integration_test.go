package poolroyalty

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Stage-2.3b detectors on real Postgres (gated on LENS_TEST_DATABASE_URL, the
// established integration pattern — runs in CI's postgres-backed job). The
// detectors are pure GROUP BY / window aggregation; pgxmock can't model that,
// so correctness is proven against a real engine on seeded rows.
func detectorTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG detector test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	// Minimal claim table (only the columns the detectors read), fresh each run.
	// DROP VIEW first: the poolroyalty integration tests share one CI database
	// and run serially in file order, so the holdback test's pool_royalty_margin
	// view (which depends on this table) may already exist — a bare DROP TABLE
	// would fail with "other objects depend on it". This keeps the detector
	// setup robust to test ordering on a shared DB.
	for _, ddl := range []string{
		`DROP VIEW IF EXISTS pool_royalty_margin`,
		`DROP TABLE IF EXISTS pool_royalty_mints`,
		`CREATE TABLE pool_royalty_mints (
			id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			request_id               TEXT NOT NULL UNIQUE,
			requester_workspace_id   TEXT NOT NULL,
			contributor_workspace_id TEXT NOT NULL,
			layer                    TEXT NOT NULL DEFAULT 'exact',
			entry_id                 TEXT NOT NULL DEFAULT '',
			provider                 TEXT NOT NULL DEFAULT '',
			model                    TEXT NOT NULL DEFAULT '',
			similarity               DOUBLE PRECISION NOT NULL DEFAULT 0,
			avoided_cogs_usd         DOUBLE PRECISION NOT NULL DEFAULT 0,
			minted_amount            DOUBLE PRECISION NOT NULL DEFAULT 0,
			answer_sha256            TEXT NOT NULL DEFAULT '',
			prompt_sha256            TEXT NOT NULL DEFAULT '',
			status                   TEXT NOT NULL DEFAULT 'final',
			finalize_after           TIMESTAMPTZ,
			created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

// seed inserts a claim row with sensible defaults; overrides via the funcs.
func seed(t *testing.T, pool *pgxpool.Pool, reqID, contributor, requester, entry, layer, status, promptHash string, similarity float64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO pool_royalty_mints
		 (request_id, requester_workspace_id, contributor_workspace_id, layer, entry_id, similarity, minted_amount, prompt_sha256, status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		reqID, requester, contributor, layer, entry, similarity, 1.0, promptHash, status)
	if err != nil {
		t.Fatalf("seed %s: %v", reqID, err)
	}
}

func thresholds() DetectorThresholds {
	return DetectorThresholds{
		VolumeMinMints: 50, VolumeMaxRequesters: 2,
		BilateralMinFrac: 0.9, BilateralMinMints: 20,
		SimilarityMinSample: 30, SimilarityMaxStddev: 0.02,
	}
}

// VOLUME: one entry hammered by a SINGLE requester (flag) vs. one entry served
// to MANY distinct requesters (legit popularity, NOT flagged).
func TestDetector_Volume_Integration(t *testing.T) {
	pool := detectorTestPool(t)
	ctx := context.Background()
	// gamed entry: 60 mints, all from wsB.
	for i := 0; i < 60; i++ {
		seed(t, pool, fmt.Sprintf("vol-gamed-%d", i), "wsA", "wsB", "entry-gamed", "exact", "final", "", 0)
	}
	// popular entry: 60 mints across 60 distinct requesters.
	for i := 0; i < 60; i++ {
		seed(t, pool, fmt.Sprintf("vol-pop-%d", i), "wsP", fmt.Sprintf("req-%d", i), "entry-popular", "exact", "final", "", 0)
	}
	r := NewDetectorReader(pool, thresholds())
	flags, err := r.VolumeConcentration(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var gamedFlagged, popularFlagged bool
	for _, f := range flags {
		if f.EntryID == "entry-gamed" && f.Flagged {
			gamedFlagged = true
			if f.DistinctRequestersOnEntry != 1 || f.EntryTotalMints != 60 {
				t.Errorf("gamed metrics: distinct=%d total=%d, want 1/60", f.DistinctRequestersOnEntry, f.EntryTotalMints)
			}
		}
		if f.EntryID == "entry-popular" && f.Flagged {
			popularFlagged = true
		}
	}
	if !gamedFlagged {
		t.Error("single-requester-hammered entry must be FLAGGED")
	}
	if popularFlagged {
		t.Error("many-distinct-requester entry is legit popularity — must NOT be flagged")
	}
}

// BILATERAL: a pair that is ~all of each side's flow (flag) vs. a pair that is
// a small fraction of each side's flow (not flagged).
func TestDetector_Bilateral_Integration(t *testing.T) {
	pool := detectorTestPool(t)
	ctx := context.Background()
	// colluding pair: 30 mints, and neither side transacts with anyone else.
	for i := 0; i < 30; i++ {
		seed(t, pool, fmt.Sprintf("bi-collude-%d", i), "cC", "rC", "e", "exact", "final", "", 0)
	}
	// diffuse contributor cD: 30 mints spread across 30 distinct requesters;
	// pair (cD, rD0) is just 1/30 of cD's flow.
	for i := 0; i < 30; i++ {
		seed(t, pool, fmt.Sprintf("bi-diffuse-%d", i), "cD", fmt.Sprintf("rD%d", i), "e", "exact", "final", "", 0)
	}
	r := NewDetectorReader(pool, thresholds())
	flags, err := r.BilateralConcentration(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var colludeFlagged bool
	for _, f := range flags {
		if f.ContributorWorkspace == "cC" && f.RequesterWorkspace == "rC" {
			colludeFlagged = f.Flagged
			if f.FracOfContributorFlow != 1.0 || f.FracOfRequesterFlow != 1.0 {
				t.Errorf("collude fracs = %v/%v, want 1.0/1.0", f.FracOfContributorFlow, f.FracOfRequesterFlow)
			}
		}
		if f.ContributorWorkspace == "cD" && f.Flagged {
			t.Errorf("diffuse pair (cD,%s) must NOT flag: fracC=%v", f.RequesterWorkspace, f.FracOfContributorFlow)
		}
	}
	if !colludeFlagged {
		t.Error("fully-bilateral pair must be FLAGGED")
	}
}

// SIMILARITY: many DISTINCT prompts at tight near-threshold similarity (flag)
// vs. organic spread (not flagged); plus the HAVING min-sample gate.
func TestDetector_Similarity_Integration(t *testing.T) {
	pool := detectorTestPool(t)
	ctx := context.Background()
	// engineered: 40 distinct prompts on one entry, all similarity ~0.905 (tight).
	for i := 0; i < 40; i++ {
		seed(t, pool, fmt.Sprintf("sim-eng-%d", i), "cE", "rE", "entry-eng", "semantic", "final",
			fmt.Sprintf("hash-%d", i), 0.905+float64(i%2)*0.001)
	}
	// organic: 40 hits on another entry, similarity spread 0.90..0.99 (wide).
	for i := 0; i < 40; i++ {
		seed(t, pool, fmt.Sprintf("sim-org-%d", i), "cO", "rO", "entry-org", "semantic", "final",
			fmt.Sprintf("ohash-%d", i), 0.90+float64(i)*0.0022)
	}
	// too-small sample: 10 tight hits — must be excluded by HAVING.
	for i := 0; i < 10; i++ {
		seed(t, pool, fmt.Sprintf("sim-small-%d", i), "cS", "rS", "entry-small", "semantic", "final",
			fmt.Sprintf("shash-%d", i), 0.905)
	}
	// SAME-PROMPT cluster: 40 hits at tight similarity but the IDENTICAL
	// prompt_sha256 every time — an organic re-ask of one popular question,
	// NOT engineered. Proves the distinct-prompt-majority gate (the other half
	// of the rule, beyond tight stddev): tight stddev alone must NOT flag when
	// the prompts are all the same.
	for i := 0; i < 40; i++ {
		seed(t, pool, fmt.Sprintf("sim-same-%d", i), "cM", "rM", "entry-same", "semantic", "final",
			"one-and-only-hash", 0.905)
	}
	r := NewDetectorReader(pool, thresholds())
	flags, err := r.SimilarityGaming(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]SimilarityFlag{}
	for _, f := range flags {
		seen[f.EntryID] = f
	}
	if eng, ok := seen["entry-eng"]; !ok || !eng.Flagged {
		t.Errorf("engineered tight cluster must be FLAGGED; got %+v ok=%v", eng, ok)
	}
	if org, ok := seen["entry-org"]; ok && org.Flagged {
		t.Errorf("organic spread must NOT be flagged; stddev=%v", org.SimStddev)
	}
	if _, ok := seen["entry-small"]; ok {
		t.Error("below-min-sample cluster must be excluded by HAVING (not returned at all)")
	}
	if same, ok := seen["entry-same"]; !ok || same.Flagged {
		t.Errorf("tight cluster of ONE repeated prompt is an organic re-ask — must NOT flag (distinct-prompt gate); got %+v ok=%v", same, ok)
	} else if same.DistinctPrompts != 1 {
		t.Errorf("same-prompt cluster: distinct_prompts=%d, want 1", same.DistinctPrompts)
	}
}

// STATUS FILTERING (critical): a REVOKED mint never appears in / inflates any
// detector; a HELD mint DOES (provisional in-window flagging).
func TestDetector_StatusFiltering_Integration(t *testing.T) {
	pool := detectorTestPool(t)
	ctx := context.Background()
	// 60 mints on one entry from one requester — but 30 are REVOKED.
	for i := 0; i < 30; i++ {
		seed(t, pool, fmt.Sprintf("st-final-%d", i), "wsA", "wsB", "entry-x", "exact", "final", "", 0)
	}
	for i := 0; i < 30; i++ {
		seed(t, pool, fmt.Sprintf("st-revoked-%d", i), "wsA", "wsB", "entry-x", "exact", "revoked", "", 0)
	}
	r := NewDetectorReader(pool, thresholds())
	flags, _ := r.VolumeConcentration(ctx, time.Hour)
	for _, f := range flags {
		if f.EntryID == "entry-x" {
			if f.EntryTotalMints != 30 {
				t.Errorf("revoked mints must NOT count: entry_total=%d, want 30 (only finals)", f.EntryTotalMints)
			}
			// 30 finals < VolumeMinMints(50) → not flagged once revoked are excluded.
			if f.Flagged {
				t.Error("with revoked excluded the entry is under the floor — must not flag")
			}
		}
	}

	// Now prove HELD rows ARE counted: add 30 HELD on a fresh entry from one
	// requester → 30... still under 50; push to 60 held to cross the floor.
	for i := 0; i < 60; i++ {
		seed(t, pool, fmt.Sprintf("st-held-%d", i), "wsA", "wsB", "entry-held",
			"exact", "held", "", 0)
	}
	flags, _ = r.VolumeConcentration(ctx, time.Hour)
	var heldFlagged bool
	for _, f := range flags {
		if f.EntryID == "entry-held" {
			if f.EntryTotalMints != 60 {
				t.Errorf("held mints MUST count (in-window detection): entry_total=%d want 60", f.EntryTotalMints)
			}
			heldFlagged = f.Flagged
		}
	}
	if !heldFlagged {
		t.Error("held mints must be eligible to flag (catching the pattern during the holdback window)")
	}
}

// Empty table ⇒ all detectors return empty without error (inert by construction).
func TestDetector_EmptyInert_Integration(t *testing.T) {
	pool := detectorTestPool(t)
	ctx := context.Background()
	r := NewDetectorReader(pool, thresholds())
	if v, err := r.VolumeConcentration(ctx, time.Hour); err != nil || len(v) != 0 {
		t.Errorf("empty volume: %v %v", v, err)
	}
	if v, err := r.BilateralConcentration(ctx, time.Hour); err != nil || len(v) != 0 {
		t.Errorf("empty bilateral: %v %v", v, err)
	}
	if v, err := r.SimilarityGaming(ctx, time.Hour); err != nil || len(v) != 0 {
		t.Errorf("empty similarity: %v %v", v, err)
	}
}
