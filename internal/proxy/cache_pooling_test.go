package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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

const okResp = `{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`

// newPoolingProxy builds a proxy with a real (miniredis) exact cache, two
// workspaces (wsA, wsB), a recording alert sink, and the pooling gate wired to a
// mutable global switch. The upstream counts calls so tests can tell a cache hit
// (upstream NOT called) from a miss (upstream called).
func newPoolingProxy(t *testing.T, global *bool) (*Proxy, *workspace.Manager, *recordingAlertSink, *cache.ExactCache, *int64) {
	t.Helper()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okResp)
	}))
	t.Cleanup(srv.Close)

	exact, _ := newExactCacheForTest(t)
	wsm := workspace.New(nil)
	for _, id := range []string{"wsA", "wsB"} {
		if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
			ID: id, Name: id, Active: true, LoggingPolicy: workspace.LoggingMetadata,
		}); err != nil {
			t.Fatal(err)
		}
	}
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, wsm, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	p.openAIURL = srv.URL
	sink := &recordingAlertSink{}
	p.setAlertSink(sink)
	p.SetPoolGate(cache_pooling.New(func() bool { return *global }, wsm.GetCachePoolable))
	return p, wsm, sink, exact, &calls
}

func dispatchWS(t *testing.T, p *Proxy, wsID, content string) *httptest.ResponseRecorder {
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
	return w
}

// OFF BY DEFAULT: with the global switch off (even though both workspaces are
// poolable), wsB cannot read wsA's entry — tenant isolation holds, upstream is
// hit again.
func TestPooling_OffByDefault_IsolationHolds(t *testing.T) {
	global := false
	p, wsm, _, _, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)

	dispatchWS(t, p, "wsA", "what is 2+2") // upstream call #1, caches
	before := atomic.LoadInt64(calls)
	dispatchWS(t, p, "wsB", "what is 2+2") // must MISS → upstream call #2
	if atomic.LoadInt64(calls)-before != 1 {
		t.Errorf("global off: wsB must NOT read wsA's cache (private isolation); upstream delta=%d want 1", atomic.LoadInt64(calls)-before)
	}
}

// ALL ON: global on + both workspaces opted in → wsB serves wsA's contributed
// entry from the pool (upstream NOT called again), and the pooled entry carries
// wsA as the recorded contributor.
func TestPooling_AllOn_CrossTenantHit(t *testing.T) {
	global := true
	p, wsm, _, exact, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)

	dispatchWS(t, p, "wsA", "what is 2+2") // upstream call #1, caches + pooled write
	before := atomic.LoadInt64(calls)
	dispatchWS(t, p, "wsB", "what is 2+2") // pooled HIT → upstream NOT called
	if atomic.LoadInt64(calls)-before != 0 {
		t.Errorf("all-on: wsB must be served from the pool (no upstream call); delta=%d want 0", atomic.LoadInt64(calls)-before)
	}
	// Provenance: the pooled entry (under the reserved pooled key) is owner-tagged
	// with wsA.
	if _, owner, _ := exact.GetWithOwner(context.Background(), "openai", "gpt-4o", pooledPromptKey("what is 2+2")); owner != "wsA" {
		t.Errorf("pooled entry must record the contributor; owner=%q want wsA", owner)
	}
}

// The pooled keyspace is PROVABLY disjoint from the workspace-private keyspace:
// a tenant cannot read a victim's PRIVATE entry by crafting a raw prompt that
// mimics the victim's "wsID:prompt" cache pre-image. This guards the opt-in
// transition leak (a private-era entry with no pooled twin).
func TestPooling_PooledKeyDisjointFromPrivate(t *testing.T) {
	exact, _ := newExactCacheForTest(t)
	// wsA's PRIVATE key for prompt "P" hashes "wsA:P".
	privateKey := exact.Key("openai", "gpt-4o", "wsA:P")
	// wsB's POOLED lookup, even with a raw prompt crafted to equal "wsA:P".
	pooledKey := exact.Key("openai", "gpt-4o", pooledPromptKey("wsA:P"))
	if privateKey == pooledKey {
		t.Fatal("pooled key must NEVER collide with a workspace-private key (cross-tenant leak)")
	}
}

// INERT/CRASH-FREE WITH NO GATE: a proxy with poolGate=nil (SetPoolGate never
// called) must serve a request through the cache write + read path without
// panicking — the nil gate's methods are nil-safe and report false.
func TestPooling_NilGate_NoPanic(t *testing.T) {
	global := true
	p, _, _, _, _ := newPoolingProxy(t, &global)
	p.SetPoolGate(nil)                     // explicitly drop the gate → poolGate is nil
	dispatchWS(t, p, "wsA", "what is 2+2") // cache MISS → storeCaches (nil-gate write path)
	dispatchWS(t, p, "wsA", "what is 2+2") // private HIT (same ws) — exercises read path
	dispatchWS(t, p, "wsB", "what is 2+2") // different ws, nil gate → no pooled read, plain miss
}

// REQUESTER NOT OPTED IN: global on + contributor (wsA) opted in, but the
// requester (wsB) is not → blocked.
func TestPooling_RequesterNotOptedIn_Blocked(t *testing.T) {
	global := true
	p, wsm, _, _, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	// wsB left non-poolable.

	dispatchWS(t, p, "wsA", "what is 2+2")
	before := atomic.LoadInt64(calls)
	dispatchWS(t, p, "wsB", "what is 2+2")
	if atomic.LoadInt64(calls)-before != 1 {
		t.Errorf("requester not opted in: must miss; upstream delta=%d want 1", atomic.LoadInt64(calls)-before)
	}
}

// CONTRIBUTOR CONSENT VERIFIED AT READ TIME: wsA contributes a pooled entry
// while poolable, then revokes its opt-in. wsB (poolable) must NOT be served the
// entry — the owner's consent is checked against the live flag, not just at write.
func TestPooling_ContributorRevoked_Blocked(t *testing.T) {
	global := true
	p, wsm, _, _, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)

	dispatchWS(t, p, "wsA", "what is 2+2")                       // wsA contributes a pooled entry (owner=wsA)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", false) // wsA revokes consent
	before := atomic.LoadInt64(calls)
	dispatchWS(t, p, "wsB", "what is 2+2")
	if atomic.LoadInt64(calls)-before != 1 {
		t.Errorf("contributor revoked: pooled hit must be blocked at read time; upstream delta=%d want 1", atomic.LoadInt64(calls)-before)
	}
}

// PII is never pooled: a request whose prompt carries PII is not cached at all
// (existing guard), so no pooled entry exists for another tenant to read.
func TestPooling_PIINeverPooled(t *testing.T) {
	global := true
	p, wsm, _, _, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)

	dispatchWS(t, p, "wsA", "email me at user@example.com") // PII → not cached, not pooled
	before := atomic.LoadInt64(calls)
	dispatchWS(t, p, "wsB", "email me at user@example.com")
	if atomic.LoadInt64(calls)-before != 1 {
		t.Errorf("PII content must never be pooled; upstream delta=%d want 1", atomic.LoadInt64(calls)-before)
	}
}

// A pooled hit is served from cache and therefore books NO spend (no ledger
// write) — consistent with every other cache hit.
func TestPooling_NoLedgerWriteOnPooledHit(t *testing.T) {
	global := true
	p, wsm, sink, _, _ := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)

	dispatchWS(t, p, "wsA", "what is 2+2") // real upstream call → 1 spend row
	before := sink.calls
	dispatchWS(t, p, "wsB", "what is 2+2") // pooled hit → zero spend
	if sink.calls-before != 0 {
		t.Errorf("a pooled cache hit must record NO spend; RecordSpend delta=%d want 0", sink.calls-before)
	}
}

// BACKWARD COMPAT: a pooled-key entry with NO recorded owner (a pre-feature
// entry) is never served cross-tenant, even with everything opted in.
func TestPooling_NoOwnerEntryNotServed(t *testing.T) {
	global := true
	p, wsm, _, exact, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)

	// Simulate a legacy pooled-key entry written WITHOUT owner provenance.
	if err := exact.Set(context.Background(), "openai", "gpt-4o", "what is 2+2", []byte(okResp)); err != nil {
		t.Fatal(err)
	}
	before := atomic.LoadInt64(calls)
	dispatchWS(t, p, "wsB", "what is 2+2")
	if atomic.LoadInt64(calls)-before != 1 {
		t.Errorf("an un-owned pooled entry must not be served (backward-compat safety); upstream delta=%d want 1", atomic.LoadInt64(calls)-before)
	}
}
