package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
)

type fixedEmbedder struct{}

func (fixedEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

// newSemPoolProxy wires a SEMANTIC cache (pgxmock) + a fixed embedder + the
// pooling gate + two workspaces; NO exact cache (so the pooled read path goes
// straight to semantic). The upstream counts calls so a hit (no call) is
// distinguishable from a miss (call).
func newSemPoolProxy(t *testing.T, global *bool) (*Proxy, *workspace.Manager, *recordingAlertSink, pgxmock.PgxPoolIface, *int64) {
	t.Helper()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okResp)
	}))
	t.Cleanup(srv.Close)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)
	semantic := cache.NewSemanticCacheWithDB(mock, fixedEmbedder{}, 0.9, time.Hour)

	wsm := workspace.New(nil)
	for _, id := range []string{"wsA", "wsB"} {
		if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
			ID: id, Name: id, Active: true, LoggingPolicy: workspace.LoggingMetadata,
		}); err != nil {
			t.Fatal(err)
		}
	}
	p := New(
		nil, semantic, fixedEmbedder{},
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, wsm, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	p.openAIURL = srv.URL
	sink := &recordingAlertSink{}
	p.setAlertSink(sink)
	p.SetPoolGate(cache_pooling.New(func() bool { return *global }, wsm.GetCachePoolable))
	return p, wsm, sink, mock, &calls
}

func dispatchSem(t *testing.T, p *Proxy, wsID, content string) {
	t.Helper()
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"` + content + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", wsID)
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ws=%s status=%d body=%s", wsID, w.Code, w.Body.String())
	}
}

// ── expectation builders mirroring serve()'s semantic-query order ──

func expPrivateMiss(m pgxmock.PgxPoolIface) {
	m.ExpectQuery(`is_poolable = false`).WithArgs(pgxmock.AnyArg(), "openai", "gpt-4o", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "response", "similarity"}))
}
func expPooledMiss(m pgxmock.PgxPoolIface) {
	m.ExpectQuery(`is_poolable = true`).WithArgs(pgxmock.AnyArg(), "openai", "gpt-4o", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "response", "contributor", "similarity"}))
}
func expPooledHit(m pgxmock.PgxPoolIface, contributor string) {
	m.ExpectQuery(`is_poolable = true`).WithArgs(pgxmock.AnyArg(), "openai", "gpt-4o", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "response", "contributor", "similarity"}).
			AddRow("row-1", okResp, contributor, 0.99))
	m.ExpectExec(`UPDATE prompt_embeddings`).WithArgs("row-1").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
}
func expPrivateStore(m pgxmock.PgxPoolIface) {
	m.ExpectExec(`INSERT INTO prompt_embeddings`).
		WithArgs("openai", "gpt-4o", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}
func expPooledStore(m pgxmock.PgxPoolIface, contributor string) {
	m.ExpectExec(`INSERT INTO prompt_embeddings`).
		WithArgs("openai", "gpt-4o", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), contributor).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

// ALL ON: wsA contributes; wsB is served wsA's entry from the semantic pool
// (no upstream call), provenance (contributor) verified live.
func TestSemanticPooling_AllOn_CrossTenantHit(t *testing.T) {
	global := true
	p, wsm, _, m, calls := newSemPoolProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)

	// wsA: private miss, pooled miss → store private + pooled.
	expPrivateMiss(m)
	expPooledMiss(m)
	expPrivateStore(m)
	expPooledStore(m, "wsA")
	// wsB: private miss, pooled HIT (owner wsA) → served, no store.
	expPrivateMiss(m)
	expPooledHit(m, "wsA")

	dispatchSem(t, p, "wsA", "what is 2+2")
	before := atomic.LoadInt64(calls)
	dispatchSem(t, p, "wsB", "what is 2+2")
	if atomic.LoadInt64(calls)-before != 0 {
		t.Errorf("all-on: wsB must be served from the semantic pool (no upstream); delta=%d", atomic.LoadInt64(calls)-before)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("query expectations: %v", err)
	}
}

// OFF BY DEFAULT: global off → no pooled read, no pooled write. wsB misses;
// the absence of any is_poolable=true query proves inertness.
func TestSemanticPooling_OffByDefault_Inert(t *testing.T) {
	global := false
	p, wsm, _, m, calls := newSemPoolProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)

	// Off → ONLY private get + private store per request (no pooled query/write).
	expPrivateMiss(m)
	expPrivateStore(m)
	expPrivateMiss(m)
	expPrivateStore(m)

	dispatchSem(t, p, "wsA", "what is 2+2")
	before := atomic.LoadInt64(calls)
	dispatchSem(t, p, "wsB", "what is 2+2")
	if atomic.LoadInt64(calls)-before != 1 {
		t.Errorf("off: wsB must miss (no cross-tenant read); delta=%d want 1", atomic.LoadInt64(calls)-before)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("off-by-default must issue NO pooled query/write: %v", err)
	}
}

// REQUESTER NOT OPTED IN: wsB is not poolable → it never issues a pooled read.
func TestSemanticPooling_RequesterNotOptedIn_Blocked(t *testing.T) {
	global := true
	p, wsm, _, m, calls := newSemPoolProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true) // contributor only

	expPrivateMiss(m)
	expPooledMiss(m)
	expPrivateStore(m)
	expPooledStore(m, "wsA")
	// wsB not poolable → NO pooled read, just private get + store.
	expPrivateMiss(m)
	expPrivateStore(m)

	dispatchSem(t, p, "wsA", "what is 2+2")
	before := atomic.LoadInt64(calls)
	dispatchSem(t, p, "wsB", "what is 2+2")
	if atomic.LoadInt64(calls)-before != 1 {
		t.Errorf("requester not opted in: must miss; delta=%d want 1", atomic.LoadInt64(calls)-before)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// CONTRIBUTOR REVOKED: wsA contributes, then revokes. wsB finds the pooled row
// but is blocked at read time (live consent), falls through to a miss.
func TestSemanticPooling_ContributorRevoked_Blocked(t *testing.T) {
	global := true
	p, wsm, _, m, calls := newSemPoolProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)

	expPrivateMiss(m)
	expPooledMiss(m)
	expPrivateStore(m)
	expPooledStore(m, "wsA")
	// wsB: private miss, pooled row FOUND (owner wsA) + touch, but consent denied
	// → miss → store (wsB is poolable, so private + pooled write).
	expPrivateMiss(m)
	expPooledHit(m, "wsA")
	expPrivateStore(m)
	expPooledStore(m, "wsB")

	dispatchSem(t, p, "wsA", "what is 2+2")
	_ = wsm.SetCachePoolable(context.Background(), "wsA", false) // revoke
	before := atomic.LoadInt64(calls)
	dispatchSem(t, p, "wsB", "what is 2+2")
	if atomic.LoadInt64(calls)-before != 1 {
		t.Errorf("contributor revoked: pooled hit blocked at read time; delta=%d want 1", atomic.LoadInt64(calls)-before)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A pooled semantic hit serves from cache → books NO spend (no ledger write).
func TestSemanticPooling_HitWritesOnlyZeroCostCacheRow(t *testing.T) {
	global := true
	p, wsm, sink, m, _ := newSemPoolProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)

	expPrivateMiss(m)
	expPooledMiss(m)
	expPrivateStore(m)
	expPooledStore(m, "wsA")
	expPrivateMiss(m)
	expPooledHit(m, "wsA")

	dispatchSem(t, p, "wsA", "what is 2+2")
	before := sink.calls
	beforeSpends := len(sink.spends)
	dispatchSem(t, p, "wsB", "what is 2+2")
	// 0099: the pooled semantic hit writes exactly ONE cache-tagged zero-cost row (hit-rate
	// visibility); an untagged row here would be a priced upstream write — a margin leak.
	if sink.calls-before != 1 {
		t.Errorf("a pooled semantic hit must record exactly ONE spend write (the tagged cache row); delta=%d", sink.calls-before)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, s := range sink.spends[beforeSpends:] {
		if s.serveSource != "cache_hit_pooled_semantic" {
			t.Errorf("semantic pooled-hit spend row serveSource = %q, want cache_hit_pooled_semantic", s.serveSource)
		}
	}
}

// PII is never pooled: a PII prompt skips the cache entirely (no semantic query
// or write at all), so nothing is contributed to the pool.
func TestSemanticPooling_PIINeverPooled(t *testing.T) {
	global := true
	p, wsm, _, m, calls := newSemPoolProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)
	// NO semantic expectations: a PII request must touch neither cache path.

	dispatchSem(t, p, "wsA", "email me at user@example.com")
	before := atomic.LoadInt64(calls)
	dispatchSem(t, p, "wsB", "email me at user@example.com")
	if atomic.LoadInt64(calls)-before != 1 {
		t.Errorf("PII content must never be pooled; delta=%d want 1", atomic.LoadInt64(calls)-before)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("PII must issue NO semantic query/write: %v", err)
	}
}

// A pooled row with NO contributor (defensive / legacy) is blocked.
func TestSemanticPooling_NoContributorBlocked(t *testing.T) {
	global := true
	p, wsm, _, m, calls := newSemPoolProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)

	// wsB: private miss, pooled row found but contributor "" → blocked → miss → store.
	expPrivateMiss(m)
	expPooledHit(m, "") // empty contributor
	expPrivateStore(m)
	expPooledStore(m, "wsB")

	before := atomic.LoadInt64(calls)
	dispatchSem(t, p, "wsB", "what is 2+2")
	if atomic.LoadInt64(calls)-before != 1 {
		t.Errorf("a pooled row with no contributor must not be served; delta=%d want 1", atomic.LoadInt64(calls)-before)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// The pooled semantic prompt_hash is provably disjoint from the private hash:
// private hashes "wsID:prompt"; pooled hashes the NUL-sentinel prompt.
func TestSemanticPooling_DisjointHash(t *testing.T) {
	priv := sha256.Sum256([]byte("openai:gpt-4o:wsA:P"))
	pool := sha256.Sum256([]byte("openai:gpt-4o:" + pooledPromptKey("P")))
	if hex.EncodeToString(priv[:]) == hex.EncodeToString(pool[:]) {
		t.Fatal("pooled semantic prompt_hash must never collide with a private one")
	}
}
