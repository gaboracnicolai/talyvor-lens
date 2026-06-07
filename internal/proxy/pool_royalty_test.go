package proxy

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/workspace"
)

// recordingRoyaltyMinter records every ServedHit the proxy reports. The
// exactly-once guarantee itself lives in poolroyalty.Minter's DB claim (tested
// there); at the proxy layer the contract under test is WHEN a hit is reported
// (serve, not lookup) and WHAT identity it carries (the request_id key).
type recordingRoyaltyMinter struct {
	mu   sync.Mutex
	hits []poolroyalty.ServedHit
}

func (r *recordingRoyaltyMinter) MintServedHit(_ context.Context, h poolroyalty.ServedHit) (poolroyalty.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hits = append(r.hits, h)
	return poolroyalty.Result{Minted: true, Amount: poolroyalty.DefaultRoyaltyShare * h.AvoidedCOGSUSD}, nil
}

func (r *recordingRoyaltyMinter) recorded() []poolroyalty.ServedHit {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]poolroyalty.ServedHit, len(r.hits))
	copy(out, r.hits)
	return out
}

// dispatchWSWithRequestID is dispatchWS plus a client-supplied
// X-Talyvor-Request-ID — the retry-stability lever for the idempotency key.
func dispatchWSWithRequestID(t *testing.T, p *Proxy, wsID, content, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"` + content + `"}]}`
	req := httptest.NewRequest("POST", "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", wsID)
	req.Header.Set("X-Talyvor-Request-ID", requestID)
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	if w.Code != 200 {
		t.Fatalf("ws=%s status=%d body=%s", wsID, w.Code, w.Body.String())
	}
	return w
}

// A SERVED cross-tenant pooled hit fires exactly one MintServedHit carrying
// the serving request's request_id (the idempotency key), the requester, the
// contributor (owner stamp), the exact layer + entry identity, and a positive
// avoided_COGS. A client retry with the SAME X-Talyvor-Request-ID reports the
// SAME key — which is what lets the DB claim dedup it to one mint.
func TestPoolRoyalty_ServedPooledHit_FiresMintKeyedOnRequestID(t *testing.T) {
	global := true
	p, wsm, _, exact, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)
	rec := &recordingRoyaltyMinter{}
	p.SetRoyaltyMinter(rec)

	dispatchWS(t, p, "wsA", "what is 2+2") // upstream #1: caches + pooled write (contributor wsA)
	if got := rec.recorded(); len(got) != 0 {
		t.Fatalf("the contributing live call must not mint; hits=%d", len(got))
	}

	before := atomic.LoadInt64(calls)
	dispatchWSWithRequestID(t, p, "wsB", "what is 2+2", "req-royalty-1") // pooled HIT, served
	if atomic.LoadInt64(calls)-before != 0 {
		t.Fatal("expected a pooled cache hit (no upstream call)")
	}

	hits := rec.recorded()
	if len(hits) != 1 {
		t.Fatalf("served pooled hit must fire exactly one mint; hits=%d", len(hits))
	}
	h := hits[0]
	if h.RequestID != "req-royalty-1" {
		t.Errorf("RequestID = %q, want the client X-Talyvor-Request-ID req-royalty-1", h.RequestID)
	}
	if h.RequesterWorkspace != "wsB" || h.ContributorWorkspace != "wsA" {
		t.Errorf("requester/contributor = %q/%q, want wsB/wsA", h.RequesterWorkspace, h.ContributorWorkspace)
	}
	if h.Layer != "exact" {
		t.Errorf("Layer = %q, want exact", h.Layer)
	}
	if want := exact.Key("openai", "gpt-4o", pooledPromptKey("what is 2+2")); h.EntryID != want {
		t.Errorf("EntryID = %q, want the pooled cache key %q", h.EntryID, want)
	}
	if h.Provider != "openai" || h.Model != "gpt-4o" {
		t.Errorf("provider/model = %q/%q, want openai/gpt-4o", h.Provider, h.Model)
	}
	if h.AvoidedCOGSUSD <= 0 {
		t.Errorf("AvoidedCOGSUSD = %v, want > 0 (the live call this hit avoided)", h.AvoidedCOGSUSD)
	}

	// Client retry with the SAME request id: the proxy reports the same key —
	// the DB UNIQUE(request_id) claim is what collapses it to one mint.
	dispatchWSWithRequestID(t, p, "wsB", "what is 2+2", "req-royalty-1")
	hits = rec.recorded()
	if len(hits) != 2 {
		t.Fatalf("retry must also be reported (DB dedups); hits=%d", len(hits))
	}
	if hits[1].RequestID != hits[0].RequestID {
		t.Errorf("retry RequestID = %q, want %q (same key → exactly-once at the claim)", hits[1].RequestID, hits[0].RequestID)
	}
}

// CLAIM AT SERVE, NOT AT LOOKUP: a pooled hit that is FOUND but cannot be
// SERVED (SSE replay fails → fall through to the live LLM) must not mint.
func TestPoolRoyalty_FoundButNotServed_DoesNotMint(t *testing.T) {
	global := true
	p, wsm, _, exact, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)
	rec := &recordingRoyaltyMinter{}
	p.SetRoyaltyMinter(rec)

	// Seed a pooled entry whose payload cannot be replayed as SSE.
	if err := exact.SetWithOwner(context.Background(), "openai", "gpt-4o",
		pooledPromptKey("stream me"), "wsA", []byte("not-a-json-payload")); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"stream me"}]}`
	req := httptest.NewRequest("POST", "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "wsB")
	req.Header.Set("X-Talyvor-Request-ID", "req-fallthrough-1")
	before := atomic.LoadInt64(calls)
	p.HandleOpenAI(httptest.NewRecorder(), req)

	if atomic.LoadInt64(calls)-before != 1 {
		t.Errorf("unreplayable pooled hit must fall through to the live LLM; upstream delta=%d want 1", atomic.LoadInt64(calls)-before)
	}
	if got := rec.recorded(); len(got) != 0 {
		t.Errorf("found-but-not-served hit must NOT mint; hits=%d (%+v)", len(got), got)
	}
}

// PRIVATE hits never mint — only the pooled (cross-tenant) layers report.
func TestPoolRoyalty_PrivateHit_DoesNotMint(t *testing.T) {
	global := true
	p, wsm, _, _, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	rec := &recordingRoyaltyMinter{}
	p.SetRoyaltyMinter(rec)

	dispatchWS(t, p, "wsA", "private question") // live
	before := atomic.LoadInt64(calls)
	dispatchWS(t, p, "wsA", "private question") // PRIVATE exact hit
	if atomic.LoadInt64(calls)-before != 0 {
		t.Fatal("expected a private cache hit")
	}
	if got := rec.recorded(); len(got) != 0 {
		t.Errorf("private hit must not mint; hits=%d", len(got))
	}
}

// INERT BY DEFAULT: with no royalty minter wired (Stage 2.0 behavior), pooled
// hits serve exactly as before — nothing panics, nothing mints.
func TestPoolRoyalty_NoMinter_PooledHitServesUnchanged(t *testing.T) {
	global := true
	p, wsm, _, _, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)
	// NO SetRoyaltyMinter — the Stage 2.0 wiring.

	dispatchWS(t, p, "wsA", "what is 2+2")
	before := atomic.LoadInt64(calls)
	dispatchWS(t, p, "wsB", "what is 2+2") // pooled hit must still serve
	if atomic.LoadInt64(calls)-before != 0 {
		t.Error("pooled hit must serve with no royalty minter wired (inert by default)")
	}
}

// STAGE 2.2 ISOLATION FROM SPEND — the critical safety property: a served
// pooled hit, with the royalty minter WIRED and minting, writes ZERO rows to
// token_events. Every customer spend reader (budgets seed/ReconcileSpent,
// workspace SpendLimitUSD enforcement, alerts.windowSpend circuit breaker,
// ROI, costanomaly, forecast, anomaly, tenant month-spend, MCP/API summaries,
// audit export) reads ONLY token_events and sums cost_usd with no row-type
// filter — so zero token_events writes proves margin cannot leak into ANY of
// them, and their results are byte-identical to pre-2.2 for the same traffic.
// Talyvor's (1−s) margin is DERIVED from pool_royalty_mints (the
// pool_royalty_margin view), never re-recorded as spend.
//
// The live-call leg is the positive control: it proves this sink DOES observe
// token_events writes when they happen, so the zero on the pooled leg is a
// real measurement, not a broken probe. (The pooled leg fires the RECORDING
// minter — the proxy-side contract; the real Minter's write targets are
// pinned separately in poolroyalty's minter_test via strict pgxmock: claim
// table + LENS ledger only, never token_events.)
func TestPoolRoyalty_ServedPooledHit_WritesNoTokenEvents(t *testing.T) {
	global := true
	p, wsm, sink, _, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)
	rec := &recordingRoyaltyMinter{}
	p.SetRoyaltyMinter(rec)

	// POSITIVE CONTROL: a live (miss) call must record spend via the sink.
	dispatchWS(t, p, "wsA", "what is 2+2")
	sink.mu.Lock()
	liveCalls := sink.calls
	sink.mu.Unlock()
	if liveCalls == 0 {
		t.Fatal("positive control failed: a live call must write a token_events spend row — the probe is broken")
	}

	// THE PROPERTY: a served pooled hit (royalty MINTED — minter recorded it)
	// adds NO token_events write. Spend-reader inputs are unchanged.
	before := atomic.LoadInt64(calls)
	dispatchWSWithRequestID(t, p, "wsB", "what is 2+2", "req-isolation-1")
	if atomic.LoadInt64(calls)-before != 0 {
		t.Fatal("expected a pooled cache hit (no upstream call)")
	}
	if got := rec.recorded(); len(got) != 1 {
		t.Fatalf("the pooled hit must have fired the royalty mint; hits=%d", len(got))
	}
	sink.mu.Lock()
	pooledCalls := sink.calls
	sink.mu.Unlock()
	if pooledCalls != liveCalls {
		t.Errorf("a pooled hit must write ZERO token_events rows: spend writes went %d → %d — margin/royalty leaked into customer spend accounting", liveCalls, pooledCalls)
	}
}

// STAGE 2.3.0 — EVIDENCE HASHES AT SERVE: a served pooled hit carries
// UNSALTED hex(sha256(...)) of the exact served bytes and the raw requester
// prompt, computed at the serve moment (the cache stores are mutable
// underneath the mint — Redis SET / semantic upsert overwrite — so only a
// serve-time hash binds the adjudicable bytes).
func TestPoolRoyalty_ServedHit_CarriesUnsaltedEvidenceHashes(t *testing.T) {
	global := true
	p, wsm, _, _, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)
	rec := &recordingRoyaltyMinter{}
	p.SetRoyaltyMinter(rec)

	dispatchWS(t, p, "wsA", "what is 2+2") // live: caches + pooled write of okResp
	before := atomic.LoadInt64(calls)
	dispatchWSWithRequestID(t, p, "wsB", "what is 2+2", "req-hash-1")
	if atomic.LoadInt64(calls)-before != 0 {
		t.Fatal("expected a pooled cache hit")
	}

	hits := rec.recorded()
	if len(hits) != 1 {
		t.Fatalf("hits=%d, want 1", len(hits))
	}
	if want := poolroyalty.SHA256Hex([]byte(okResp)); hits[0].AnswerSHA256 != want {
		t.Errorf("AnswerSHA256 = %q, want unsalted sha256 of the served bytes %q", hits[0].AnswerSHA256, want)
	}
	if want := poolroyalty.SHA256Hex([]byte("what is 2+2")); hits[0].PromptSHA256 != want {
		t.Errorf("PromptSHA256 = %q, want unsalted sha256 of the raw prompt %q", hits[0].PromptSHA256, want)
	}
}

// STAGE 2.3.0 — NO HASH → NO MINT, the privacy-coherence gate at the proxy:
// a requester whose LoggingPolicy is 'none' forbids persisting content-
// derived artifacts, so the serve captures no hashes and fires NO mint at
// all — but the request itself still serves from the pool (customer gets
// the answer + savings; there is simply no royalty event). Defense in depth:
// the Minter independently refuses empty-hash hits (tested in poolroyalty).
func TestPoolRoyalty_NoneLoggingPolicy_ServesButNeverMints(t *testing.T) {
	global := true
	p, wsm, _, _, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	// wsNone: pooling participant whose logging policy forbids evidence capture.
	if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: "wsNone", Name: "wsNone", Active: true, LoggingPolicy: workspace.LoggingNone,
	}); err != nil {
		t.Fatal(err)
	}
	_ = wsm.SetCachePoolable(context.Background(), "wsNone", true)
	rec := &recordingRoyaltyMinter{}
	p.SetRoyaltyMinter(rec)

	dispatchWS(t, p, "wsA", "what is 2+2") // live: seeds the pooled entry
	before := atomic.LoadInt64(calls)
	dispatchWSWithRequestID(t, p, "wsNone", "what is 2+2", "req-none-1") // pooled hit must still SERVE
	if atomic.LoadInt64(calls)-before != 0 {
		t.Fatal("none-policy requester must still be served from the pool (no upstream call)")
	}
	if got := rec.recorded(); len(got) != 0 {
		t.Errorf("none-policy serve must fire NO mint (no hash → no mint); hits=%d (%+v)", len(got), got)
	}
}
