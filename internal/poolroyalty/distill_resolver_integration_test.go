package poolroyalty

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PR3 — the distill resolver. The PRECISION test is the money-safety property: a
// resolve returns EXACTLY the held request_ids matching the pattern — no clean decoy
// (false inclusion → wrong clawback of an innocent contributor) and no missing gaming
// row (false exclusion). Reuses distillMintHarness.

// distillReqID is the once-per-relationship key the minter derives (and the adjudicate
// endpoint's revoke_request_ids).
func distillReqID(owner, requester, content string) string {
	return SHA256Hex([]byte(owner + ":" + requester + ":" + content))
}

// seedDistillStatusMint inserts one distill_royalty_mints row with an explicit status
// + finalize_after (the resolver requires status='held' AND finalize_after IS NOT NULL).
func seedDistillStatusMint(t *testing.T, pool *pgxpool.Pool, owner, requester, content, status string, finalizeAfter *time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO distill_royalty_mints
		   (request_id, contributor_workspace_id, requester_workspace_id, content_hash, avoided_cogs_usd, minted_amount, status, finalize_after)
		 VALUES ($1,$2,$3,$4,2.0,1.0,$5,$6) ON CONFLICT (request_id) DO NOTHING`,
		distillReqID(owner, requester, content), owner, requester, content, status, finalizeAfter); err != nil {
		t.Fatalf("seed %s/%s/%s: %v", owner, requester, content, err)
	}
}

func candidateRIDSet(cands []Candidate) map[string]bool {
	m := make(map[string]bool, len(cands))
	for _, c := range cands {
		m[c.RequestID] = true
	}
	return m
}

func assertRIDSet(t *testing.T, name string, got map[string]bool, want ...string) {
	t.Helper()
	wantSet := make(map[string]bool, len(want))
	for _, w := range want {
		wantSet[w] = true
		if !got[w] {
			t.Errorf("%s: FALSE EXCLUSION — expected held request_id missing from candidates: %s", name, w)
		}
	}
	for g := range got {
		if !wantSet[g] {
			t.Errorf("%s: FALSE INCLUSION — unexpected request_id in candidates: %s", name, g)
		}
	}
	if len(got) != len(wantSet) {
		t.Errorf("%s: got %d candidates, want %d", name, len(got), len(wantSet))
	}
}

// PRECISION — exact match against a table holding a swarm + a tight pair + clean decoys
// + a FINAL + a REVOKED row. The decoys/final/revoked must NEVER appear.
func TestDistillResolver_Precision_Integration(t *testing.T) {
	pool, _ := distillMintHarness(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)

	// SWARM: content "hot", owner wsA, 3 distinct requesters (held).
	seedDistillStatusMint(t, pool, "wsA", "r1", "hot", "held", &future)
	seedDistillStatusMint(t, pool, "wsA", "r2", "hot", "held", &future)
	seedDistillStatusMint(t, pool, "wsA", "r3", "hot", "held", &future)
	// TIGHT PAIR: owner wsC, requester wsD, 2 contents (held).
	seedDistillStatusMint(t, pool, "wsC", "wsD", "c1", "held", &future)
	seedDistillStatusMint(t, pool, "wsC", "wsD", "c2", "held", &future)
	// CLEAN DECOYS — must NEVER be returned:
	seedDistillStatusMint(t, pool, "wsA", "r1", "other", "held", &future)  // wsA but a DIFFERENT content → not in the "hot" swarm
	seedDistillStatusMint(t, pool, "wsX", "wsD", "z", "held", &future)     // wsD requester but a DIFFERENT owner → not in (wsC,wsD)
	seedDistillStatusMint(t, pool, "wsC", "wsZ", "c3", "held", &future)    // wsC owner but a DIFFERENT requester → not in (wsC,wsD)
	seedDistillStatusMint(t, pool, "wsA", "r4", "hot", "final", &future)   // FINAL on "hot" → not revocable (held-only)
	seedDistillStatusMint(t, pool, "wsC", "wsD", "c9", "revoked", &future) // REVOKED on the pair → not returned

	res := NewDistillResolver(pool)

	// VOLUME swarm — EXACTLY hot's 3 held rows; label content_swarm.
	vr, err := res.ResolveVolume(ctx, DistillVolumeFlag{ContentHash: "hot", ContributorWorkspace: "wsA"}, time.Hour)
	if err != nil {
		t.Fatalf("ResolveVolume: %v", err)
	}
	if vr.Label != LabelContentSwarm {
		t.Errorf("volume label=%q want content_swarm", vr.Label)
	}
	assertRIDSet(t, "volume-swarm", candidateRIDSet(vr.Candidates),
		distillReqID("wsA", "r1", "hot"), distillReqID("wsA", "r2", "hot"), distillReqID("wsA", "r3", "hot"))
	for _, c := range vr.Candidates {
		if c.Status != "held" { // held-only + adjudicate-ready (the revoker CAS targets status='held')
			t.Errorf("volume candidate %s status=%q want held", c.RequestID, c.Status)
		}
	}

	// SELF_DEALING pair — EXACTLY (wsC,wsD)'s 2 held rows; label pair_coarse.
	sr, err := res.ResolveSelfDealing(ctx, DistillSelfDealingFlag{ContributorWorkspace: "wsC", RequesterWorkspace: "wsD"}, time.Hour)
	if err != nil {
		t.Fatalf("ResolveSelfDealing: %v", err)
	}
	if sr.Label != LabelPairCoarse {
		t.Errorf("self_dealing label=%q want pair_coarse", sr.Label)
	}
	assertRIDSet(t, "self-dealing-pair", candidateRIDSet(sr.Candidates),
		distillReqID("wsC", "wsD", "c1"), distillReqID("wsC", "wsD", "c2"))
}

// LOOP CONNECTS — every returned request_id is a HELD row that the distill adjudicate
// endpoint's revoke_request_ids → the revoker CAS (WHERE request_id=$1 AND status='held')
// targets. So detect → resolve → adjudicate chains by construction.
func TestDistillResolver_HeldOnly_LoopConnects_Integration(t *testing.T) {
	pool, _ := distillMintHarness(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	seedDistillStatusMint(t, pool, "wsA", "r1", "k", "held", &future)
	seedDistillStatusMint(t, pool, "wsA", "r2", "k", "final", &future)   // excluded
	seedDistillStatusMint(t, pool, "wsA", "r3", "k", "revoked", &future) // excluded

	res := NewDistillResolver(pool)
	vr, err := res.ResolveVolume(ctx, DistillVolumeFlag{ContentHash: "k", ContributorWorkspace: "wsA"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	assertRIDSet(t, "held-only", candidateRIDSet(vr.Candidates), distillReqID("wsA", "r1", "k"))
	// the one returned id == the once-per-relationship key the adjudicate endpoint consumes
	if len(vr.Candidates) == 1 && vr.Candidates[0].RequestID != distillReqID("wsA", "r1", "k") {
		t.Errorf("request_id %q != adjudicate revoke_request_ids key", vr.Candidates[0].RequestID)
	}
}

// TestDistillResolver_NoWriteMethods — the type-level read-only guarantee (mirrors
// TestResolver_NoWriteMethods): the db seam exposes no write primitive.
func TestDistillResolver_NoWriteMethods(t *testing.T) {
	dbField, ok := reflect.TypeOf(DistillResolver{}).FieldByName("db")
	if !ok {
		t.Fatal("DistillResolver must hold its db via a 'db' field")
	}
	forbidden := []string{"Exec", "Begin", "BeginTx", "SendBatch", "CopyFrom", "Prepare"}
	for i := 0; i < dbField.Type.NumMethod(); i++ {
		name := dbField.Type.Method(i).Name
		for _, bad := range forbidden {
			if name == bad {
				t.Errorf("DistillResolver.db exposes forbidden write method %q — read-only seam violated", name)
			}
		}
	}
}

func TestDistillResolver_NilInert(t *testing.T) {
	var r *DistillResolver
	if res, err := r.ResolveVolume(context.Background(), DistillVolumeFlag{}, time.Hour); err != nil || res.Candidates != nil {
		t.Fatalf("nil ResolveVolume must be inert; got %v %v", res, err)
	}
	if res, err := r.ResolveSelfDealing(context.Background(), DistillSelfDealingFlag{}, time.Hour); err != nil || res.Candidates != nil {
		t.Fatalf("nil ResolveSelfDealing must be inert; got %v %v", res, err)
	}
}
