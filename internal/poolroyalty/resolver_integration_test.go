package poolroyalty

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func resolverTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG resolver test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	entryCapResetSchema(t, pool, ctx) // reuse the full claim-table schema + index
	return pool
}

// insertMint seeds a claim row directly (status + similarity + finalize_after
// chosen by the test). created_at defaults to now().
func insertMint(t *testing.T, pool *pgxpool.Pool, reqID, contributor, requester, entry, layer, status string, similarity float64, finalizeAfter time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO pool_royalty_mints
		 (request_id, requester_workspace_id, contributor_workspace_id, layer, entry_id, similarity, minted_amount, status, finalize_after)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		reqID, requester, contributor, layer, entry, similarity, 1.0, status, finalizeAfter)
	if err != nil {
		t.Fatalf("insert %s: %v", reqID, err)
	}
}

// HELD-ONLY: a resolver surfaces ONLY held rows — final and revoked never
// appear, even when they match the flag's identifiers exactly.
func TestResolver_HeldOnly_Integration(t *testing.T) {
	pool := resolverTestPool(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	insertMint(t, pool, "h1", "wsA", "wsB", "e1", "exact", "held", 0, future)
	insertMint(t, pool, "f1", "wsA", "wsB", "e1", "exact", "final", 0, future)
	insertMint(t, pool, "r1", "wsA", "wsB", "e1", "exact", "revoked", 0, future)

	r := NewResolver(pool)
	res, err := r.ResolveVolume(ctx, VolumeFlag{EntryID: "e1", ContributorWorkspace: "wsA", RequesterWorkspace: "wsB"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].RequestID != "h1" {
		t.Fatalf("held-only: got %+v, want exactly [h1] (final + revoked must NOT surface)", res.Candidates)
	}
	if res.Candidates[0].Status != "held" {
		t.Errorf("candidate status = %q, want held", res.Candidates[0].Status)
	}
}

// OVER-SELECTION HONESTY (Volume): a LEGITIMATE organic held mint on the SAME
// (entry, contributor, requester) tuple as a flagged one comes back as a
// candidate TOO — the resolver does NOT claim to distinguish fraud from honest
// traffic; the label tuple_pinned says so.
func TestResolver_OverSelectionHonesty_Volume_Integration(t *testing.T) {
	pool := resolverTestPool(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	insertMint(t, pool, "gamed", "wsA", "wsB", "e1", "exact", "held", 0, future)
	insertMint(t, pool, "organic", "wsA", "wsB", "e1", "exact", "held", 0, future) // legit, same tuple

	r := NewResolver(pool)
	res, _ := r.ResolveVolume(ctx, VolumeFlag{EntryID: "e1", ContributorWorkspace: "wsA", RequesterWorkspace: "wsB"}, time.Hour)
	if len(res.Candidates) != 2 {
		t.Fatalf("BOTH the gamed and the organic mint on the same tuple must be candidates (resolver is honest about over-selection); got %d", len(res.Candidates))
	}
	if res.Label != LabelTuplePinned {
		t.Errorf("label = %q, want tuple_pinned (the honest over-selection claim)", res.Label)
	}
}

// OVER-SELECTION HONESTY (SelfDealing): the pair resolver sweeps in held mints
// across DIFFERENT entries for the same workspace pair — including legitimate
// ones on an unrelated entry — and labels pair_coarse.
func TestResolver_OverSelectionHonesty_SelfDealing_Integration(t *testing.T) {
	pool := resolverTestPool(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	insertMint(t, pool, "sd-a", "cX", "rX", "entry-1", "exact", "held", 0, future)
	insertMint(t, pool, "sd-b", "cX", "rX", "entry-2", "exact", "held", 0, future) // different entry, same pair

	r := NewResolver(pool)
	res, _ := r.ResolveSelfDealing(ctx, SelfDealingFlag{ContributorWorkspace: "cX", RequesterWorkspace: "rX"}, time.Hour)
	if len(res.Candidates) != 2 {
		t.Fatalf("pair-coarse must sweep ALL held mints for the pair across entries; got %d, want 2", len(res.Candidates))
	}
	if res.Label != LabelPairCoarse {
		t.Errorf("label = %q, want pair_coarse", res.Label)
	}
}

// SIMILARITY NARROWING: with a band, only in-band held rows are candidates and
// the label is similarity_narrowed; an out-of-band held row on the same
// (contributor, entry) is excluded by the band.
func TestResolver_SimilarityNarrowed_Integration(t *testing.T) {
	pool := resolverTestPool(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	insertMint(t, pool, "in-1", "cS", "rS", "eS", "semantic", "held", 0.905, future) // in band
	insertMint(t, pool, "in-2", "cS", "rS", "eS", "semantic", "held", 0.912, future) // in band
	insertMint(t, pool, "out", "cS", "rS", "eS", "semantic", "held", 0.99, future)   // out of band (organic-wide)

	r := NewResolver(pool)
	res, _ := r.ResolveSimilarity(ctx, SimilarityFlag{ContributorWorkspace: "cS", EntryID: "eS", SimMin: 0.90, SimMax: 0.92}, time.Hour)
	if len(res.Candidates) != 2 {
		t.Fatalf("narrowed: only in-band rows; got %d, want 2 (the 0.99 row excluded)", len(res.Candidates))
	}
	if res.Label != LabelSimilarityNarrowed {
		t.Errorf("label = %q, want similarity_narrowed", res.Label)
	}

	// Without a usable band → fallback selects all 3 (contributor,entry) held rows.
	res2, _ := r.ResolveSimilarity(ctx, SimilarityFlag{ContributorWorkspace: "cS", EntryID: "eS", SimMin: 0, SimMax: 0}, time.Hour)
	if len(res2.Candidates) != 3 || res2.Label != LabelSimilarityUnnarrowed {
		t.Fatalf("unnarrowed fallback: got %d cands / label %q, want 3 / similarity_unnarrowed", len(res2.Candidates), res2.Label)
	}
}

// TIMELEFT / PASTWINDOW / WINDOW-AGE: a held row close to finalize_after shows
// a small positive TimeLeft; a held row PAST finalize_after (sweeper hasn't run)
// shows TimeLeft==0 AND PastWindow==true (the most urgent candidate); a row
// older than the resolver window is not selected.
func TestResolver_TimeLeftAndPastWindow_Integration(t *testing.T) {
	pool := resolverTestPool(t)
	ctx := context.Background()
	now := time.Now()
	insertMint(t, pool, "soon", "wsA", "wsB", "eT", "exact", "held", 0, now.Add(2*time.Minute)) // ~2m left
	insertMint(t, pool, "past", "wsA", "wsB", "eT", "exact", "held", 0, now.Add(-time.Minute))  // past window, still held

	r := NewResolver(pool)
	res, _ := r.ResolveVolume(ctx, VolumeFlag{EntryID: "eT", ContributorWorkspace: "wsA", RequesterWorkspace: "wsB"}, time.Hour)
	if len(res.Candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(res.Candidates))
	}
	byID := map[string]Candidate{}
	for _, c := range res.Candidates {
		byID[c.RequestID] = c
	}
	soon := byID["soon"]
	if soon.PastWindow || soon.TimeLeft <= 0 || soon.TimeLeft > 3*time.Minute {
		t.Errorf("soon: PastWindow=%v TimeLeft=%v, want false / small positive", soon.PastWindow, soon.TimeLeft)
	}
	past := byID["past"]
	if !past.PastWindow {
		t.Errorf("past: PastWindow=%v, want true (revocable RIGHT NOW, racing the sweeper)", past.PastWindow)
	}
	if past.TimeLeft != 0 {
		t.Errorf("past: TimeLeft=%v, want 0 (clamped, never negative)", past.TimeLeft)
	}

	// A held row OLDER than the resolver window is not selected.
	old := now.Add(-48 * time.Hour)
	_, err := pool.Exec(ctx, `INSERT INTO pool_royalty_mints
		(request_id, requester_workspace_id, contributor_workspace_id, layer, entry_id, similarity, minted_amount, status, finalize_after, created_at)
		VALUES ('old','wsB','wsA','exact','eT',0,1.0,'held',$1,$2)`, now.Add(time.Hour), old)
	if err != nil {
		t.Fatal(err)
	}
	res2, _ := r.ResolveVolume(ctx, VolumeFlag{EntryID: "eT", ContributorWorkspace: "wsA", RequesterWorkspace: "wsB"}, time.Hour)
	for _, c := range res2.Candidates {
		if c.RequestID == "old" {
			t.Error("a held row older than the window must not be selected")
		}
	}
}
