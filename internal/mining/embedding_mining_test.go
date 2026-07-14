package mining

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newMockEmbMiner(t *testing.T) (*EmbeddingMiner, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return NewEmbeddingMiner(newLedgerStore(mock), mock), mock
}

// ─── EmbeddingEarningRate ────────────────────────

func TestEmbeddingEarningRate_PerModel(t *testing.T) {
	// All for 1000 embeddings — should produce base × multiplier.
	cases := map[string]int64{ // µLENS (SEC-2)
		"nomic-embed-text":       micro(0.002), // small
		"e5-large":               micro(0.003), // medium
		"mxbai-embed-large":      micro(0.004), // large
		"text-embedding-3-small": micro(0.002),
		"text-embedding-3-large": micro(0.004),
	}
	for model, expected := range cases {
		t.Run(model, func(t *testing.T) {
			got := EmbeddingEarningRate(model, 1000)
			if got != expected { // integer µLENS — exact
				t.Fatalf("model=%q expected %d, got %d µLENS", model, expected, got)
			}
		})
	}
}

func TestEmbeddingEarningRate_ScalesWithCount(t *testing.T) {
	r1 := EmbeddingEarningRate("nomic-embed-text", 1000)
	r2 := EmbeddingEarningRate("nomic-embed-text", 5000)
	if r2 != r1*5 {
		t.Fatalf("expected 5× scaling: %d vs %d", r1, r2)
	}
	if EmbeddingEarningRate("nomic-embed-text", 0) != 0 {
		t.Fatal("zero count should earn zero")
	}
}

func TestEmbeddingEarningRate_UnknownModelFallsBackToSmall(t *testing.T) {
	got := EmbeddingEarningRate("unknown-model-xyz", 1000)
	want := MulFloor(EmbeddingMineBaseRate, ModelMultiplierSmall) // µLENS
	if got != want {
		t.Fatalf("unknown model should fall back to small, got %d want %d", got, want)
	}
}

// ─── RegisterNode validation ─────────────────────

func TestRegisterEmbeddingNode_RejectsInvalidURL(t *testing.T) {
	miner, _ := newMockEmbMiner(t)
	_, err := miner.RegisterNode(context.Background(), EmbeddingNode{
		WorkspaceID: "ws",
		URL:         "ftp://bad",
		Model:       "nomic-embed-text",
		Dimensions:  768,
	})
	if !errors.Is(err, ErrInvalidNodeURL) {
		t.Fatalf("expected ErrInvalidNodeURL, got %v", err)
	}
}

func TestRegisterEmbeddingNode_RejectsUnknownModel(t *testing.T) {
	miner, _ := newMockEmbMiner(t)
	_, err := miner.RegisterNode(context.Background(), EmbeddingNode{
		WorkspaceID: "ws",
		URL:         "http://x:1",
		Model:       "fake-embed",
		Dimensions:  1536,
	})
	if !errors.Is(err, ErrInvalidEmbeddingModel) {
		t.Fatalf("expected ErrInvalidEmbeddingModel, got %v", err)
	}
}

func TestRegisterEmbeddingNode_RejectsInvalidDimensions(t *testing.T) {
	miner, _ := newMockEmbMiner(t)
	_, err := miner.RegisterNode(context.Background(), EmbeddingNode{
		WorkspaceID: "ws",
		URL:         "http://x:1",
		Model:       "nomic-embed-text",
		Dimensions:  512,
	})
	if !errors.Is(err, ErrInvalidEmbeddingDimensions) {
		t.Fatalf("expected ErrInvalidEmbeddingDimensions, got %v", err)
	}
}

// ─── RecordEmbeddingsServed ──────────────────────

func expectGetEmbeddingNode(mock pgxmock.PgxPoolIface, n EmbeddingNode) {
	mock.ExpectQuery("SELECT id, workspace_id, url, model, dimensions").
		WithArgs(n.ID).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "model", "dimensions",
			"max_batch", "speed_tps", "active", "verified", "created_at",
		}).AddRow(
			n.ID, n.WorkspaceID, n.URL, n.Model, n.Dimensions,
			n.MaxBatch, n.SpeedTPS, n.Active, n.Verified, n.CreatedAt,
		))
}

// TestRecordEmbeddingsServed_CreditsOwner moved to the real-PG held-routing
// integration test (traffic_held_node_mints_integration_test.go, incl. the
// e5-large medium-tier rate) when Phase-3 Item 1 routed the embedding mint
// through CreditOnceHeld — held vs spendable is a DB behavior proven on a real
// engine. The self-serve skip below stays a fast mock test (no ledger touch).
func TestRecordEmbeddingsServed_SkipsSelfServing(t *testing.T) {
	miner, mock := newMockEmbMiner(t)
	node := EmbeddingNode{
		ID: "e_self", WorkspaceID: "ws_self", URL: "http://x", Model: "nomic-embed-text",
		Dimensions: 768, MaxBatch: 100, SpeedTPS: 200,
		Active: true, Verified: true, CreatedAt: time.Now(),
	}
	expectGetEmbeddingNode(mock, node)
	// No Credit expectation — owner == requester must not earn.
	if err := miner.RecordEmbeddingsServed(context.Background(),
		"e_self", "ws_self", "req-1", 1000); err != nil {
		t.Fatalf("RecordEmbeddingsServed: %v", err)
	}
}

func TestRecordEmbeddingsServed_ZeroCountSkip(t *testing.T) {
	miner, _ := newMockEmbMiner(t)
	// No expectations — must short-circuit without touching DB.
	if err := miner.RecordEmbeddingsServed(context.Background(), "node", "ws", "req-1", 0); err != nil {
		t.Fatalf("RecordEmbeddingsServed: %v", err)
	}
}

// ─── ListAvailableNodes ──────────────────────────

func TestListAvailableEmbeddingNodes_FiltersByModelAndDimensions(t *testing.T) {
	miner, mock := newMockEmbMiner(t)
	mock.ExpectQuery("WHERE verified = TRUE AND active = TRUE").
		WithArgs("nomic-embed-text", 768).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "model", "dimensions",
			"max_batch", "speed_tps", "active", "verified", "created_at",
		}).
			AddRow("e1", "ws1", "http://a", "nomic-embed-text", 768,
				64, 800, true, true, time.Now()).
			AddRow("e2", "ws2", "http://b", "nomic-embed-text", 1024,
				128, 600, true, true, time.Now()))
	nodes, err := miner.ListAvailableNodes(context.Background(), "nomic-embed-text", 768)
	if err != nil {
		t.Fatalf("ListAvailableNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	for _, n := range nodes {
		if !n.Verified || !n.Active {
			t.Fatalf("expected verified+active, got %+v", n)
		}
		if n.Model != "nomic-embed-text" {
			t.Fatalf("expected model match, got %s", n.Model)
		}
	}
}

// ─── GetStats ────────────────────────────────────

func TestGetStats_ReturnsCorrectTotals(t *testing.T) {
	miner, mock := newMockEmbMiner(t)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM embedding_nodes").
		WithArgs("ws_s").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(\\(metadata->>'embedding_count'\\)").
		WithArgs("ws_s", TypeEmbeddingMine).
		WillReturnRows(pgxmock.NewRows([]string{"embeddings", "amount"}).
			AddRow(int64(50_000), micro(0.15)))
	mock.ExpectQuery("SELECT COALESCE\\(MIN\\(created_at\\)").
		WithArgs("ws_s", TypeEmbeddingMine).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(time.Now().Add(-15 * 24 * time.Hour)))
	stats, err := miner.GetStats(context.Background(), "ws_s")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.NodesActive != 3 || stats.EmbeddingsServed != 50_000 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if stats.TotalEarned != micro(0.15) {
		t.Fatalf("expected %d µLENS earned, got %d", micro(0.15), stats.TotalEarned)
	}
	if stats.EstimatedMonthly <= 0 {
		t.Fatalf("expected positive monthly projection, got %d", stats.EstimatedMonthly)
	}
}

// ─── DeactivateEmbeddingNode ─────────────────────

func TestDeactivateEmbeddingNode_NotFound(t *testing.T) {
	miner, mock := newMockEmbMiner(t)
	mock.ExpectExec("UPDATE embedding_nodes SET active = FALSE").
		WithArgs("ghost", "ws-x").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	if err := miner.DeactivateEmbeddingNode(context.Background(), "ghost", "ws-x"); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("expected ErrNodeNotFound, got %v", err)
	}
}

// ─── RegisterNode DB path ────────────────────────

// TestRegisterEmbeddingNode_StoresSecretHash verifies that a pre-bcrypt'd
// NodeSecretHash is forwarded as the 7th INSERT argument (ISO 27001 A.9).
// The HTTP handler performs the bcrypt step; RegisterNode is responsible
// only for persisting what it receives.
func TestRegisterEmbeddingNode_StoresSecretHash(t *testing.T) {
	miner, mock := newMockEmbMiner(t)

	const (
		wsID     = "ws_hash"
		nodeURL  = "http://embed-host:11434"
		model    = "nomic-embed-text"
		dims     = 768
		fakeHash = "$2a$10$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZ01"
	)

	mock.ExpectQuery("INSERT INTO embedding_nodes").
		WithArgs(wsID, nodeURL, model, dims, 100, 500, fakeHash).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).
			AddRow("emb_test_hash", time.Now()))

	node, err := miner.RegisterNode(context.Background(), EmbeddingNode{
		WorkspaceID:    wsID,
		URL:            nodeURL,
		Model:          model,
		Dimensions:     dims,
		NodeSecretHash: fakeHash,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if node.ID != "emb_test_hash" {
		t.Fatalf("expected emb_test_hash, got %s", node.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRegisterEmbeddingNode_EmptySecretPassesEmptyString verifies that when
// no secret is supplied, the 7th INSERT arg is "" — Postgres NULLIF converts
// that to NULL so node_secret_hash is stored as NULL (backward compat).
func TestRegisterEmbeddingNode_EmptySecretPassesEmptyString(t *testing.T) {
	miner, mock := newMockEmbMiner(t)

	mock.ExpectQuery("INSERT INTO embedding_nodes").
		WithArgs("ws_nosec", "http://embed:11434", "e5-large", 1024, 100, 500, "").
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).
			AddRow("emb_test_null", time.Now()))

	node, err := miner.RegisterNode(context.Background(), EmbeddingNode{
		WorkspaceID: "ws_nosec",
		URL:         "http://embed:11434",
		Model:       "e5-large",
		Dimensions:  1024,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if node.ID != "emb_test_null" {
		t.Fatalf("expected emb_test_null, got %s", node.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ─── EmbeddingRates ──────────────────────────────

func TestEmbeddingRates_ExposesKnownModels(t *testing.T) {
	r := EmbeddingRates()
	models, ok := r["models"].(map[string]float64)
	if !ok {
		t.Fatal("expected models map")
	}
	for _, m := range []string{
		"nomic-embed-text", "e5-large", "mxbai-embed-large",
		"text-embedding-3-small", "text-embedding-3-large",
	} {
		if _, ok := models[m]; !ok {
			t.Fatalf("missing model %q in rates", m)
		}
	}
}
