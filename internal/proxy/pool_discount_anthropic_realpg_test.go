package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/workspace"
)

// THE POOLED-HIT DISCOUNT, PROVEN OVER THE REAL SERVE PATH ON /anthropic, AGAINST REAL POSTGRES.
//
// ⚠ WHY THIS FILE EXISTS, AND WHY THE EXISTING TESTS DID NOT CATCH THE LIVE FAILURE.
//
// #383's tests are unit tests: PricePooledHit, SavingHeaders, SetPoolConsumerDiscount. They assert
// a function's arithmetic. Not one of them drives an HTTP request through serve(), and the pooled
// tests that DO drive HTTP (cache_pooling_test.go) go through HandleOpenAI and assert cache
// isolation, not money. So the whole feature could vanish from the serve path with every test
// still green — which is exactly what happened: #384 reverted #383 wholesale, deleting the code
// AND its tests together, and CI passed because a consistent revert is consistent.
//
// A test that asserts a function is right cannot notice that nothing calls it.
//
// So this one goes end to end on the route the live request used: two workspaces, both poolable,
// a cross-tenant pooled hit through HandleAnthropic, and the assertion is THE LEDGER ROW — the
// consumer's actual bill, which is the artifact the complaint was about.

const anthropicPooledPrompt = "what is the capital of france"

// anthropicPoolProxy wires a full proxy on a real-PG reservation store with pooling ON and both
// workspaces opted in, pointed at an Anthropic-shaped upstream.
func anthropicPoolProxy(t *testing.T) (*Proxy, *pgxpool.Pool, *int64) {
	t.Helper()
	_, store, pool := seamProxy(t) // real-PG ledger + reservation schema
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	p.router = nil // deterministic served model
	// The money path, wired exactly as costWireProxy wires it: without the spender and the
	// reservation there is no hold and no settle, so nothing reaches lxc_ledger and this file
	// would be asserting on an empty table.
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })

	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		// Anthropic's shape, with the usage block the pricing path reads: 10 in / 45 out, the
		// live numbers from the report.
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant",`+
			`"content":[{"type":"text","text":"Paris."}],`+
			`"model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":45}}`)
	}))
	t.Cleanup(up.Close)
	p.anthropicURL = up.URL

	wsm := p.workspaceManager
	for _, id := range []string{"wsPoolA", "wsPoolB"} {
		if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
			ID: id, Name: id, Active: true, LoggingPolicy: workspace.LoggingMetadata,
		}); err != nil {
			t.Fatal(err)
		}
		if err := wsm.SetCachePoolable(context.Background(), id, true); err != nil {
			t.Fatal(err)
		}
		seamFund(t, pool, id, costWireFunded)
	}
	on := true
	p.SetPoolGate(cache_pooling.New(func() bool { return on }, wsm.GetCachePoolable))
	// ⚠ WIRED FROM THE CONFIG DEFAULT, exactly as cmd/lens/main.go wires it. Using the same
	// constant the deployment uses is the point: a test that invents its own rate proves the
	// arithmetic and not the deployed behaviour, and the live failure was a WIRING failure.
	cfg, err := config.Load()
	if err != nil {
		// Load() requires several unrelated env vars; fall back to the documented default rather
		// than skip, and say so, so this can never silently test r=0.
		p.SetPoolConsumerDiscount(0.30)
	} else {
		p.SetPoolConsumerDiscount(cfg.PoolConsumerDiscount)
	}
	return p, pool, &calls
}

func anthropicRequest(t *testing.T, p *Proxy, ws string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"` + anthropicPooledPrompt + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", ws)
	// A scoped key, so the reservation/settle path runs (it keys the hold on the agent id).
	req = req.WithContext(auth.WithAuthContext(req.Context(),
		&auth.AuthContext{APIKeyID: "agent-" + ws, WorkspaceID: ws}))
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ws=%s status=%d body=%s", ws, w.Code, w.Body.String())
	}
	return w
}

// spendRow reads the newest delivered-charge row for a workspace: the amount actually debited and
// the metadata document beside it. This is the consumer's evidence — a function returning the
// right number while the row records the old one is the failure this must exclude.
func spendRow(t *testing.T, pool *pgxpool.Pool, ws string) (int64, map[string]any) {
	t.Helper()
	var amount int64
	var raw []byte
	err := pool.QueryRow(context.Background(),
		`SELECT amount, COALESCE(metadata,'{}'::jsonb) FROM lxc_ledger
		  WHERE workspace_id = $1 AND amount < 0 ORDER BY id DESC LIMIT 1`, ws).Scan(&amount, &raw)
	if err != nil {
		t.Fatalf("no delivered-charge row for %s: %v", ws, err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("metadata is not a JSON document: %v", err)
	}
	return amount, meta
}

// TestPooledHitOnAnthropic_ChargesTheDiscountedPrice is the live failure, reproduced.
//
// Reported: 10 in / 45 out on claude-haiku-4-5 charged 2350 µLXC — exactly list price — with no
// `saved` field on the row. At the default r=0.30 the consumer should pay ~1645.
func TestPooledHitOnAnthropic_ChargesTheDiscountedPrice(t *testing.T) {
	p, pool, calls := anthropicPoolProxy(t)

	// wsPoolA misses and populates the shared entry.
	anthropicRequest(t, p, "wsPoolA")
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("first request hit upstream %d times, want 1", got)
	}
	listCharge, _ := spendRow(t, pool, "wsPoolA")

	// wsPoolB takes the CROSS-TENANT POOLED hit — no upstream call.
	anthropicRequest(t, p, "wsPoolB")
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("the pooled request called upstream (%d total) — it was not served from the "+
			"shared cache, so this test is not exercising a pooled hit at all", got)
	}

	pooledCharge, meta := spendRow(t, pool, "wsPoolB")

	// ⚠ "CHEAPER THAN THE UNCACHED SERVE" IS NOT THE ASSERTION, and a first draft of this test
	// used it and passed on the bug. A pooled hit is already slightly cheaper for an unrelated
	// reason — the cache-aware cost basis prices cached input below uncached — so on the broken
	// build wsPoolB paid 2170 against wsPoolA's 2350: 7.7% less, and nothing to do with the
	// discount, which should have taken 30%. An inequality against a DIFFERENT request's charge
	// cannot tell the two apart.
	//
	// So the assertion is RECONCILIATION on the row itself: saved must be present and positive,
	// and charge + saved must equal the list price this hit would have cost. Those three numbers
	// come from one row, so they cannot drift apart, and no incidental pricing difference can
	// satisfy them.
	rawSaved, ok := meta["pool_saved_ulxc"]
	if !ok {
		t.Fatalf("the delivered-charge row carries no `pool_saved_ulxc`; metadata = %v.\n"+
			"Without it the consumer cannot tell a discounted charge from list price, which is "+
			"the complaint that started this. (Uncached serve charged %d µLXC; this pooled hit "+
			"charged %d.)", meta, -listCharge, -pooledCharge)
	}
	saved, okNum := rawSaved.(float64)
	if !okNum || saved <= 0 {
		t.Fatalf("`pool_saved_ulxc` = %v, want a positive number (metadata = %v)", rawSaved, meta)
	}
	charged := float64(-pooledCharge)
	list := charged + saved
	// The row states the list price itself; it must agree with charged+saved or the three
	// numbers on one row do not add up.
	if lp, ok := meta["pool_list_ulxc"].(float64); ok && lp != list {
		t.Errorf("pool_list_ulxc = %.0f but charged+saved = %.0f — the row does not reconcile", lp, list)
	}
	rate := saved / list
	if rate < 0.25 || rate > 0.35 {
		t.Errorf("the discount rate on the row is %.3f (charged %.0f, saved %.0f, list %.0f), "+
			"want ~0.30 — the configured default", rate, charged, saved, list)
	}
}

// TestPooledHitOnAnthropic_IsActuallyPooled is the premise check.
//
// If the second request were served from wsPoolB's OWN cache, or missed and called upstream, the
// discount assertion above would be measuring the wrong thing. Assert the serve source recorded
// for the request, so a green result cannot come from a non-pooled path.
func TestPooledHitOnAnthropic_IsActuallyPooled(t *testing.T) {
	p, _, calls := anthropicPoolProxy(t)
	anthropicRequest(t, p, "wsPoolA")
	rec := anthropicRequest(t, p, "wsPoolB")

	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("upstream called %d times across two requests, want 1 — the second was not a "+
			"cache hit", got)
	}
	if src := rec.Header().Get("X-Talyvor-Cache"); src != "" && !strings.Contains(src, "hit") {
		t.Errorf("X-Talyvor-Cache = %q on the second request, want a hit", src)
	}
}
