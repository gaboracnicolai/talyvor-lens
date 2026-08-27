package mining

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// verified_flag_meaning_realpg_test.go — WHAT `verified` PROVES, EXECUTED.
//
// `inference_nodes.verified` / `embedding_nodes.verified` is the node registry's
// only "is this node real" flag. It is written in exactly one place per table
// (verifyNodeAsync / the embedding sibling) and gated on in exactly one query per
// table — ListAvailableNodes, which feeds the anonymous discovery route. What it
// MEANS therefore decides what that flag is worth, and reading verifyNodeAsync is
// not a measurement of it. This drives the shipped probe and records the answer.
//
// The answer, pinned below rather than asserted in prose: `verified = TRUE` means
// SOMETHING AT THE REGISTERED URL ANSWERED HTTP 200 TO AN UNAUTHENTICATED GET.
// Not that the registrant controls the host. Not that it can serve the models it
// claims. Not that it is the software it says it is. Each of those is a separate
// test below, and each of them currently records "no".
//
// ⚠ THIS TEST CHANGES NOTHING AND ASSERTS NO POLICY. Deciding what verification
// SHOULD mean is a product decision, and the obvious repair — a challenge the node
// signs with the ed25519 key it already registers — is a protocol change. What is
// merged here is the fact, so the decision can be taken against a measurement.
//
// ⚠ AND WHAT IS *NOT* CLAIMED, MEASURED SO THE RECORD IS HONEST: the SERVING path
// is not left unguarded by this. localrouter.Router.probe applies a byte-identical
// check (same per-provider paths, same 200-only rule) freshly on every health
// sweep and gates routing on its own Healthy flag, and gateway auto-routing is
// opt-in (LENS_NODE_AUTOROUTE_ENABLED). `verified` is redundant there, not missing.
//
// Gated on LENS_TEST_DATABASE_URL.

// probeRecorder is an httptest server standing in for a registered node. It
// records every inbound request so the test can assert what Lens presented.
type probeRecorder struct {
	mu     sync.Mutex
	reqs   []*http.Request
	status int
	body   string
	srv    *httptest.Server
}

func newProbeRecorder(t *testing.T, status int, body string) *probeRecorder {
	t.Helper()
	p := &probeRecorder{status: status, body: body}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.reqs = append(p.reqs, r.Clone(context.Background()))
		p.mu.Unlock()
		w.WriteHeader(p.status)
		_, _ = w.Write([]byte(p.body))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *probeRecorder) seen() []*http.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*http.Request(nil), p.reqs...)
}

// verifiedHarness is a real-PG fixture carrying the two registry tables
// RegisterNode writes, plus node_metrics (registration seeds it in the same tx,
// so without it every registration rolls back and every assertion below would be
// measuring a failed insert rather than a probe).
type verifiedHarness struct{ pool *pgxpool.Pool }

func newVerifiedHarness(t *testing.T) *verifiedHarness {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG verified-flag meaning test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS node_metrics`,
		`DROP TABLE IF EXISTS inference_nodes`,
		`DROP TABLE IF EXISTS embedding_nodes`,
		`CREATE TABLE inference_nodes (
			id              TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
			workspace_id    TEXT NOT NULL,
			url             TEXT NOT NULL,
			provider        TEXT NOT NULL,
			models          TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
			gpu_type        TEXT NOT NULL DEFAULT 'cpu',
			max_concurrent  INTEGER NOT NULL DEFAULT 1,
			price_per_token DOUBLE PRECISION NOT NULL DEFAULT 0.050,
			active          BOOLEAN NOT NULL DEFAULT TRUE,
			verified        BOOLEAN NOT NULL DEFAULT FALSE,
			ed25519_pubkey  TEXT,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE node_metrics (
			node_id         TEXT PRIMARY KEY REFERENCES inference_nodes(id) ON DELETE CASCADE,
			requests_served INTEGER NOT NULL DEFAULT 0,
			tokens_served   BIGINT NOT NULL DEFAULT 0)`,
		`CREATE TABLE embedding_nodes (
			id               TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
			workspace_id     TEXT NOT NULL,
			url              TEXT NOT NULL,
			model            TEXT NOT NULL,
			dimensions       INTEGER NOT NULL DEFAULT 1536,
			max_batch        INTEGER NOT NULL DEFAULT 100,
			speed_tps        INTEGER NOT NULL DEFAULT 500,
			active           BOOLEAN NOT NULL DEFAULT TRUE,
			verified         BOOLEAN NOT NULL DEFAULT FALSE,
			node_secret_hash TEXT,
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return &verifiedHarness{pool: pool}
}

// waitVerifiedPool polls the row until verified flips or the deadline passes.
// verifyNodeAsync is a fire-and-forget goroutine that nothing waits for, so
// polling is the honest way to observe it; the returned bool is the final state.
func waitVerifiedPool(t *testing.T, table, nodeID string, harness *verifiedHarness, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	var v bool
	for {
		if err := harness.pool.QueryRow(context.Background(),
			`SELECT verified FROM `+table+` WHERE id = $1`, nodeID).Scan(&v); err != nil {
			t.Fatalf("read verified: %v", err)
		}
		if v || time.Now().After(deadline) {
			return v
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestVerifiedMeansOnlyThatSomethingAnswered200(t *testing.T) {
	h := newVerifiedHarness(t)

	// A host that answers 200 on /health and NOTHING else about it is true: it is
	// not an inference server, it serves no models, and it presented no proof that
	// whoever registered it controls it.
	node := newProbeRecorder(t, http.StatusOK, `not an inference server`)

	m := &ComputeMiner{pool: h.pool, httpClient: node.srv.Client()}
	got, err := m.RegisterNode(context.Background(), InferenceNode{
		WorkspaceID: "ws-registrant", URL: node.srv.URL,
		Provider: "llamacpp", Models: []string{"llama-3-70b"}, GPUType: "a100",
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	if !waitVerifiedPool(t, "inference_nodes", got.ID, h, 5*time.Second) {
		t.Fatal("the node never became verified — then this test is not measuring what " +
			"verification means, it is measuring a probe that did not run")
	}

	// (1) THE PROBE PRESENTED NO CREDENTIAL. This is the whole "no ownership proof"
	// claim, executed rather than read off the source.
	reqs := node.seen()
	if len(reqs) == 0 {
		t.Fatal("the stand-in node received no request at all, yet the row is verified — " +
			"the flag was set by something other than the probe")
	}
	for _, r := range reqs {
		for _, hdr := range []string{"Authorization", "X-Node-Secret", "X-Api-Key", "X-Talyvor-Key"} {
			if v := r.Header.Get(hdr); v != "" {
				t.Errorf("the probe sent %s=%q — if it authenticates, this test's premise is "+
					"stale and the finding must be re-measured", hdr, v)
			}
		}
		if r.Method != http.MethodGet {
			t.Errorf("probe method = %s, want GET", r.Method)
		}
	}

	// (2) THE BODY WAS NOT READ FOR MEANING. The stand-in returned prose, not a
	// model list, and the node is verified anyway.
	if body := node.body; body == "" {
		t.Fatal("the fixture returned an empty body, so 'the body is not checked' is untested")
	}
}

func TestVerifiedIsNotSetWhenTheHostRefuses(t *testing.T) {
	h := newVerifiedHarness(t)

	// CONTROL — the probe is not a no-op that flips the flag regardless. A host
	// that answers 404 must leave the row unverified. Without this, the test above
	// would pass on a probe that ignored the response entirely.
	node := newProbeRecorder(t, http.StatusNotFound, `nope`)

	m := &ComputeMiner{pool: h.pool, httpClient: node.srv.Client()}
	got, err := m.RegisterNode(context.Background(), InferenceNode{
		WorkspaceID: "ws-registrant", URL: node.srv.URL,
		Provider: "llamacpp", Models: []string{"llama-3-70b"}, GPUType: "a100",
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if waitVerifiedPool(t, "inference_nodes", got.ID, h, 2*time.Second) {
		t.Fatal("a host answering 404 was marked verified — the status check is inert")
	}
	if len(node.seen()) == 0 {
		t.Fatal("the stand-in node was never probed, so 'stayed unverified' proves nothing " +
			"about the status check")
	}
}

func TestVerifiedDoesNotProveTheNodeServesTheModelsItClaims(t *testing.T) {
	h := newVerifiedHarness(t)

	// The registrant claims two models. The host answers 200 and says nothing about
	// either. It is verified regardless — so `verified` carries no statement about
	// capability, which matters because the discovery route publishes the claimed
	// model list beside the flag.
	node := newProbeRecorder(t, http.StatusOK, `{}`)

	m := &ComputeMiner{pool: h.pool, httpClient: node.srv.Client()}
	got, err := m.RegisterNode(context.Background(), InferenceNode{
		WorkspaceID: "ws-registrant", URL: node.srv.URL,
		Provider: "llamacpp",
		Models:   []string{"gpt-4o", "claude-3-opus"},
		GPUType:  "h100",
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if !waitVerifiedPool(t, "inference_nodes", got.ID, h, 5*time.Second) {
		t.Fatal("node not verified — the measurement did not run")
	}

	var models []string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT models FROM inference_nodes WHERE id = $1`, got.ID).Scan(&models); err != nil {
		t.Fatalf("read models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("stored models = %v, want the two the registrant claimed", models)
	}
	// The record: a verified node advertising two models it was never asked about.
	t.Logf("verified=true for a host that answered 200 and claims %v — capability unchecked", models)
}

func TestVerifiedEmbeddingSiblingHasTheSameMeaning(t *testing.T) {
	h := newVerifiedHarness(t)
	node := newProbeRecorder(t, http.StatusOK, `not an embedding server`)

	m := &EmbeddingMiner{pool: h.pool, httpClient: node.srv.Client()}
	got, err := m.RegisterNode(context.Background(), EmbeddingNode{
		WorkspaceID: "ws-registrant", URL: node.srv.URL,
		Model: "e5-large", Dimensions: 1024, MaxBatch: 8, SpeedTPS: 100,
	})
	if err != nil {
		t.Fatalf("RegisterNode (embedding): %v", err)
	}
	if !waitVerifiedPool(t, "embedding_nodes", got.ID, h, 5*time.Second) {
		t.Fatal("embedding node never verified — the sibling probe did not run")
	}
	for _, r := range node.seen() {
		if v := r.Header.Get("Authorization"); v != "" {
			t.Errorf("the embedding probe sent Authorization=%q", v)
		}
	}
}
