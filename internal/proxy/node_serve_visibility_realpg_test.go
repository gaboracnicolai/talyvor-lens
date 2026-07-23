package proxy

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/povi"
	"github.com/talyvor/lens/internal/workspace"
)

// A registered inference node that answers /inference with a fixed node response. It ignores the
// node-auth token (the gateway signs one, but a fake node need not verify it to exercise the
// gateway's OWN recording path — the thing under test).
func fakeInferenceNode(t *testing.T, text string, inTok, outTok int) *httptest.Server {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"text": text, "input_tokens": inTok, "output_tokens": outTok})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wireNode registers a healthy endpoint for `model` pointing at `nodeURL` and enables auto-route.
func wireNode(t *testing.T, p *Proxy, model, nodeURL string, client *http.Client) {
	t.Helper()
	r := localrouter.NewRouter(nil)
	r.Register(&localrouter.LocalEndpoint{
		ID: "node-1", URL: nodeURL, Provider: "vllm", Models: []string{model}, Healthy: true,
	})
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	p.SetNodeRouter(r, priv, client, true)
}

// THE HOLE, reproduced then closed: a node-served request currently writes NO token_events row
// (tryNodeRouting records to the learner, not this table), so it is absent from the cache hit-rate
// denominator. After the fix it writes exactly one row tagged serve_source='node' with cost_usd
// EXACTLY zero — and the hit-rate query counts it as a MISS. Asserted on the row, never a status code.
func TestNodeServeVisibility_RealPG_WritesNodeRow_CountsAsMiss(t *testing.T) {
	pool := cacheVisPool(t)
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata) // registers ws-log (LoggingMetadata)
	p.setAlertSink(alerts.New(pool, nil, nil))               // real PG-backed manager

	node := fakeInferenceNode(t, "the node produced these output bytes right here", 9, 7)
	wireNode(t, p, "node-model", node.URL, node.Client())

	const reqID = "node-req-vis-1"
	rec := httptest.NewRecorder()
	served := p.tryNodeRouting(rec, context.Background(),
		"vllm", "node-model", "a prompt of a certain length here", "a prompt of a certain length here",
		"ws-log", "", "", "feat", "sess", reqID, false, "", localrouter.RoutingStrategy(""))
	if !served {
		t.Fatalf("tryNodeRouting returned false — node serve did not happen (status=%d body=%s)", rec.Code, rec.Body.String())
	}

	// THE ROW.
	var source string
	var cost float64
	var inT, outT int
	if err := pool.QueryRow(context.Background(),
		`SELECT serve_source, cost_usd, input_tokens, output_tokens FROM token_events WHERE request_id=$1`, reqID).
		Scan(&source, &cost, &inT, &outT); err != nil {
		t.Fatalf("node serve wrote NO token_events row (the denominator hole): %v", err)
	}
	if source != "node" {
		t.Errorf("serve_source = %q, want 'node'", source)
	}
	if cost != 0 {
		t.Errorf("cost_usd = %v, want EXACTLY 0 — Talyvor paid no provider for a node serve (any LENS owed is lens_token_ledger's number, a different ledger + unit)", cost)
	}
	if inT <= 0 || outT <= 0 {
		t.Errorf("tokens = %d/%d, want both > 0 (Lens-measured len/4)", inT, outT)
	}

	// COUNTS AS A MISS: the same hit-rate query /v1/api/usage runs. With one cache hit alongside the
	// node serve, the node row must be in the denominator but NOT the numerator.
	if err := alerts.New(pool, nil, nil).RecordCacheServe(context.Background(),
		"ws-log", "", "", "feat", "node-model", 4, 3, "sess", "cachehit-req-1", "text", "cache_hit_exact"); err != nil {
		t.Fatalf("seed cache hit: %v", err)
	}
	var hits, total int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FILTER (WHERE serve_source LIKE 'cache_hit%'), COUNT(*)
		 FROM token_events WHERE request_id = ANY($1)`, []string{reqID, "cachehit-req-1"}).Scan(&hits, &total); err != nil {
		t.Fatalf("hit-rate query: %v", err)
	}
	if hits != 1 || total != 2 {
		t.Errorf("hit-rate substrate = %d/%d, want 1/2 — the node serve is a MISS in the denominator, not a hit", hits, total)
	}
}

// PoVI INTERACTION: both writes happen at the node path. The mint-basis measurement (migration 0099,
// PK request_id ON CONFLICT DO NOTHING) and the new token_events 'node' row live in DIFFERENT tables.
// Confirm the new write cannot double-count the measurement, contradict it, or change what a receipt
// mints against: after a node serve, served_request_measurements has exactly one row (the mint basis,
// node_id + measured output) and token_events has exactly one 'node' row — disjoint, consistent.
func TestNodeServe_PoVIInteraction_MeasurementAndTokenEventDisjoint(t *testing.T) {
	pool := cacheVisPool(t)
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	p.setAlertSink(alerts.New(pool, nil, nil))
	p.SetServedMeasurementRecorder(povi.NewMeasurementStore(pool)) // wire the mint-basis writer (0099)

	node := fakeInferenceNode(t, "node output text used to size the measured token count", 5, 11)
	wireNode(t, p, "node-model", node.URL, node.Client())

	const reqID = "node-req-interaction-1"
	rec := httptest.NewRecorder()
	if !p.tryNodeRouting(rec, context.Background(),
		"vllm", "node-model", "prompt", "prompt", "ws-log", "", "", "feat", "sess", reqID,
		false, "", localrouter.RoutingStrategy("")) {
		t.Fatalf("node serve did not happen: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The mint basis (served_request_measurements): exactly one row, bound to the node, carrying the
	// gateway-measured output tokens — UNCHANGED by the token_events write (different table).
	var mNode string
	var mCount, mOut int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*), MAX(node_id), MAX(output_tokens) FROM served_request_measurements WHERE request_id=$1`, reqID).
		Scan(&mCount, &mNode, &mOut); err != nil {
		t.Fatalf("measurement read: %v", err)
	}
	if mCount != 1 || mNode != "node-1" || mOut <= 0 {
		t.Fatalf("served_request_measurements = count %d node %q out %d, want 1 / node-1 / >0 (the mint basis)", mCount, mNode, mOut)
	}

	// The hit-rate/spend row (token_events): exactly one 'node' row for the same request — a SEPARATE
	// table, so it cannot double-count or alter the measurement the receipt mints against.
	var teCount int
	var teSource string
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*), MAX(serve_source) FROM token_events WHERE request_id=$1`, reqID).Scan(&teCount, &teSource); err != nil {
		t.Fatalf("token_events read: %v", err)
	}
	if teCount != 1 || teSource != "node" {
		t.Fatalf("token_events = count %d source %q, want 1 / 'node'", teCount, teSource)
	}
}
