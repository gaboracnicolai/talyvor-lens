package poolroyalty

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// THE LOAD-BEARING SAFETY TEST: the Resolver's db handle exposes ONLY read
// methods (Query/QueryRow) — no Exec, no Begin, no SendBatch, no CopyFrom. A
// resolver that cannot reach a write primitive cannot revoke, slash, or mutate
// any row — write-impossibility is a compile-time guarantee, not a convention.
// Mirrors TestDetectorReader_NoWriteMethods. Fails if anyone widens resolverDB.
func TestResolver_NoWriteMethods(t *testing.T) {
	dbField, ok := reflect.TypeOf(Resolver{}).FieldByName("db")
	if !ok {
		t.Fatal("Resolver must hold its db via a 'db' field")
	}
	forbidden := []string{"Exec", "Begin", "BeginTx", "SendBatch", "CopyFrom", "Prepare"}
	for i := 0; i < dbField.Type.NumMethod(); i++ {
		name := dbField.Type.Method(i).Name
		for _, bad := range forbidden {
			if name == bad {
				t.Errorf("resolverDB exposes write method %q — the resolver must be read-only by construction (it produces candidates, it can never revoke)", name)
			}
		}
	}
	for _, need := range []string{"Query", "QueryRow"} {
		if _, has := dbField.Type.MethodByName(need); !has {
			t.Errorf("resolverDB missing read method %q", need)
		}
	}
}

func newResolverMock(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func candidateRows() *pgxmock.Rows {
	// request_id, contributor, minted_amount, created_at, finalize_after, status, similarity, past_window, time_left_secs
	now := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	return pgxmock.NewRows([]string{
		"request_id", "contributor_workspace_id", "minted_amount", "created_at", "finalize_after", "status", "similarity", "past_window", "time_left_secs",
	}).AddRow("req-a", "wsA", 1.0, now, now.Add(time.Hour), "held", 0.91, false, 3600.0)
}

// VOLUME: WHERE pins entry+contributor+requester+held; label tuple_pinned.
func TestResolveVolume_QueryAndLabel(t *testing.T) {
	pool := newResolverMock(t)
	r := NewResolver(pool)
	pool.ExpectQuery(`WHERE entry_id = \$1\s+AND contributor_workspace_id = \$2\s+AND requester_workspace_id = \$3\s+AND status = 'held'`).
		WithArgs("e1", "wsA", "wsB", (24 * time.Hour).Microseconds()).
		WillReturnRows(candidateRows())

	res, err := r.ResolveVolume(context.Background(), VolumeFlag{EntryID: "e1", ContributorWorkspace: "wsA", RequesterWorkspace: "wsB"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Label != LabelTuplePinned {
		t.Errorf("label = %q, want tuple_pinned", res.Label)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].RequestID != "req-a" {
		t.Errorf("candidates = %+v", res.Candidates)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// SELF-DEALING: WHERE is the workspace PAIR only (no entry_id) — coarsest;
// label pair_coarse.
func TestResolveSelfDealing_QueryAndLabel(t *testing.T) {
	pool := newResolverMock(t)
	r := NewResolver(pool)
	pool.ExpectQuery(`WHERE contributor_workspace_id = \$1\s+AND requester_workspace_id = \$2\s+AND status = 'held'`).
		WithArgs("wsA", "wsB", (24 * time.Hour).Microseconds()).
		WillReturnRows(candidateRows())

	res, err := r.ResolveSelfDealing(context.Background(), SelfDealingFlag{ContributorWorkspace: "wsA", RequesterWorkspace: "wsB"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Label != LabelPairCoarse {
		t.Errorf("label = %q, want pair_coarse", res.Label)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// SIMILARITY with a usable band → applies similarity BETWEEN min/max, label
// similarity_narrowed.
func TestResolveSimilarity_NarrowedWithBand(t *testing.T) {
	pool := newResolverMock(t)
	r := NewResolver(pool)
	pool.ExpectQuery(`AND layer = 'semantic'\s+AND status = 'held'.*AND similarity BETWEEN \$4 AND \$5`).
		WithArgs("wsA", "e1", (24 * time.Hour).Microseconds(), 0.90, 0.92).
		WillReturnRows(candidateRows())

	res, err := r.ResolveSimilarity(context.Background(),
		SimilarityFlag{ContributorWorkspace: "wsA", EntryID: "e1", SimMin: 0.90, SimMax: 0.92}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Label != LabelSimilarityNarrowed {
		t.Errorf("label = %q, want similarity_narrowed", res.Label)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// SIMILARITY without a usable band (SimMax == 0) → falls back to the
// unnarrowed query (no BETWEEN), label similarity_unnarrowed.
func TestResolveSimilarity_UnnarrowedFallback(t *testing.T) {
	pool := newResolverMock(t)
	r := NewResolver(pool)
	pool.ExpectQuery(`AND layer = 'semantic'\s+AND status = 'held'`).
		WithArgs("wsA", "e1", (24 * time.Hour).Microseconds()).
		WillReturnRows(candidateRows())

	res, err := r.ResolveSimilarity(context.Background(),
		SimilarityFlag{ContributorWorkspace: "wsA", EntryID: "e1", SimMin: 0, SimMax: 0}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Label != LabelSimilarityUnnarrowed {
		t.Errorf("label = %q, want similarity_unnarrowed", res.Label)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// Nil-safe: a nil resolver returns an empty result, no panic.
func TestResolver_NilSafe(t *testing.T) {
	var r *Resolver
	if res, err := r.ResolveVolume(context.Background(), VolumeFlag{}, time.Hour); err != nil || res.Candidates != nil {
		t.Errorf("nil ResolveVolume = %+v, %v; want empty/nil", res, err)
	}
	if res, err := r.ResolveSelfDealing(context.Background(), SelfDealingFlag{}, time.Hour); err != nil || res.Candidates != nil {
		t.Errorf("nil ResolveSelfDealing = %+v, %v", res, err)
	}
	if res, err := r.ResolveSimilarity(context.Background(), SimilarityFlag{}, time.Hour); err != nil || res.Candidates != nil {
		t.Errorf("nil ResolveSimilarity = %+v, %v", res, err)
	}
}
