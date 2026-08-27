package mining

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// available_nodes_crosstenant_realpg_test.go — the PREMISE behind the public
// discovery projection, EXECUTED rather than read.
//
// ListAvailableNodes is what feeds GET /v1/nodes/available and
// GET /v1/embedding-nodes/available, both of which sit in main.go's no-auth
// `pub` group. cmd/lens/public_node_discovery_test.go proves what the HANDLER
// puts on the wire; that proof is only worth something if the STORE really does
// hand it rows belonging to other tenants, with url and workspace_id populated.
// Reading the SQL is not that proof — a filter can be present in the text and
// inert in the plan. So this runs it.
//
// It also runs the two filters in the query as CONTROLS, because "returns
// everything" and "returns nothing" both make the leak test above pass for the
// wrong reason: an unverified node and an inactive node must NOT come back, and
// if they do the `WHERE verified = TRUE AND active = TRUE` clause is inert and
// the population this route publishes is larger than anyone thinks.
//
// Gated on LENS_TEST_DATABASE_URL, like every other real-PG test here.

func availableNodesHarness(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG available-nodes cross-tenant test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	// Mirrors migrations 0020 (inference_nodes) and 0021 (embedding_nodes) for
	// exactly the columns ListAvailableNodes selects. node_metrics references
	// inference_nodes, so drop it first when it exists.
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
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE embedding_nodes (
			id            TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
			workspace_id  TEXT NOT NULL,
			url           TEXT NOT NULL,
			model         TEXT NOT NULL,
			dimensions    INTEGER NOT NULL DEFAULT 1536,
			max_batch     INTEGER NOT NULL DEFAULT 100,
			speed_tps     INTEGER NOT NULL DEFAULT 500,
			active        BOOLEAN NOT NULL DEFAULT TRUE,
			verified      BOOLEAN NOT NULL DEFAULT FALSE,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func seedInferenceNode(t *testing.T, pool *pgxpool.Pool, id, ws, url, model string, verified, active bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO inference_nodes (id, workspace_id, url, provider, models, gpu_type, verified, active)
		 VALUES ($1,$2,$3,'vllm',ARRAY[$4]::TEXT[],'a100',$5,$6)`,
		id, ws, url, model, verified, active); err != nil {
		t.Fatalf("seed inference_node %s: %v", id, err)
	}
}

func seedEmbeddingNode(t *testing.T, pool *pgxpool.Pool, id, ws, url, model string, verified, active bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO embedding_nodes (id, workspace_id, url, model, dimensions, verified, active)
		 VALUES ($1,$2,$3,$4,1024,$5,$6)`,
		id, ws, url, model, verified, active); err != nil {
		t.Fatalf("seed embedding_node %s: %v", id, err)
	}
}

func TestListAvailableNodes_ReturnsEveryTenantsRowsWithURLAndOwner(t *testing.T) {
	pool := availableNodesHarness(t)
	ctx := context.Background()

	seedInferenceNode(t, pool, "n-a", "ws-A", "http://gpu-a.internal:8080", "llama-3-70b", true, true)
	seedInferenceNode(t, pool, "n-b", "ws-B", "https://10.4.4.9:9443", "llama-3-70b", true, true)

	m := &ComputeMiner{pool: pool}
	got, err := m.ListAvailableNodes(ctx, "llama-3-70b")
	if err != nil {
		t.Fatalf("ListAvailableNodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 — the store is not returning both tenants' nodes, so "+
			"neither the leak nor its fix is what this measures", len(got))
	}
	byID := map[string]InferenceNode{}
	for _, n := range got {
		byID[n.ID] = n
	}
	// The point: a query with no caller identity in it returns rows owned by
	// somebody, and it fills in who and where.
	for _, c := range []struct{ id, ws, url string }{
		{"n-a", "ws-A", "http://gpu-a.internal:8080"},
		{"n-b", "ws-B", "https://10.4.4.9:9443"},
	} {
		n, ok := byID[c.id]
		if !ok {
			t.Fatalf("row %s missing from the result", c.id)
		}
		if n.WorkspaceID != c.ws {
			t.Errorf("%s: WorkspaceID = %q, want %q", c.id, n.WorkspaceID, c.ws)
		}
		if n.URL != c.url {
			t.Errorf("%s: URL = %q, want %q", c.id, n.URL, c.url)
		}
	}
}

// CONTROL — the two filters in the query are live, not decorative. If either
// stops binding, this route's published population silently grows to include
// nodes nobody probed and nodes their owner switched off.
func TestListAvailableNodes_VerifiedAndActiveFiltersActuallyBind(t *testing.T) {
	pool := availableNodesHarness(t)
	ctx := context.Background()

	seedInferenceNode(t, pool, "n-ok", "ws-A", "http://ok:8080", "m1", true, true)
	seedInferenceNode(t, pool, "n-unverified", "ws-B", "http://unverified:8080", "m1", false, true)
	seedInferenceNode(t, pool, "n-inactive", "ws-C", "http://inactive:8080", "m1", true, false)

	m := &ComputeMiner{pool: pool}
	got, err := m.ListAvailableNodes(ctx, "m1")
	if err != nil {
		t.Fatalf("ListAvailableNodes: %v", err)
	}
	if len(got) != 1 || got[0].ID != "n-ok" {
		ids := make([]string, 0, len(got))
		for _, n := range got {
			ids = append(ids, n.ID)
		}
		t.Fatalf("got %v, want exactly [n-ok] — `WHERE verified = TRUE AND active = TRUE` is not "+
			"binding, so the public discovery route publishes unprobed or switched-off nodes", ids)
	}
}

// CONTROL — the model filter binds too, so "returns everything" cannot be the
// reason the cross-tenant test above saw two rows.
func TestListAvailableNodes_ModelFilterActuallyBinds(t *testing.T) {
	pool := availableNodesHarness(t)
	ctx := context.Background()

	seedInferenceNode(t, pool, "n-m1", "ws-A", "http://a:8080", "m1", true, true)
	seedInferenceNode(t, pool, "n-m2", "ws-B", "http://b:8080", "m2", true, true)

	m := &ComputeMiner{pool: pool}
	got, err := m.ListAvailableNodes(ctx, "m2")
	if err != nil {
		t.Fatalf("ListAvailableNodes: %v", err)
	}
	if len(got) != 1 || got[0].ID != "n-m2" {
		t.Fatalf("model filter did not bind: got %d rows", len(got))
	}
}

func TestListAvailableEmbeddingNodes_ReturnsEveryTenantsRowsWithURLAndOwner(t *testing.T) {
	pool := availableNodesHarness(t)
	ctx := context.Background()

	seedEmbeddingNode(t, pool, "e-a", "ws-A", "http://embed-a.internal:7000", "bge-large", true, true)
	seedEmbeddingNode(t, pool, "e-b", "ws-B", "https://192.168.7.7:7443", "bge-large", true, true)
	seedEmbeddingNode(t, pool, "e-unverified", "ws-C", "http://nope:7000", "bge-large", false, true)

	m := &EmbeddingMiner{pool: pool}
	got, err := m.ListAvailableNodes(ctx, "bge-large", 512)
	if err != nil {
		t.Fatalf("ListAvailableNodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (the unverified one must be filtered out)", len(got))
	}
	for _, n := range got {
		if n.WorkspaceID == "" || n.URL == "" {
			t.Errorf("row %s came back without owner/url (ws=%q url=%q) — then the leak this "+
				"guards against would not be reachable and this test is not measuring it",
				n.ID, n.WorkspaceID, n.URL)
		}
	}
}
