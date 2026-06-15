package mining

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newMockMiner(t *testing.T) (*ComputeMiner, *LedgerStore, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	ledger := newLedgerStore(mock)
	return NewComputeMiner(ledger, mock), ledger, mock
}

// ─── EarningRate ─────────────────────────────────

func TestEarningRate_PerGPU(t *testing.T) {
	cases := []struct {
		gpu      string
		expected float64
	}{
		{"cpu", 0.025},      // 0.050 × 0.5
		{"rtx4090", 0.050},  // 0.050 × 1.0
		{"a100", 0.100},     // 0.050 × 2.0
		{"h100", 0.150},     // 0.050 × 3.0
	}
	for _, c := range cases {
		t.Run(c.gpu, func(t *testing.T) {
			got := EarningRate(c.gpu, 1000)
			diff := got - c.expected
			if diff < 0 {
				diff = -diff
			}
			if diff > 1e-9 {
				t.Fatalf("expected %f, got %f", c.expected, got)
			}
		})
	}
}

func TestEarningRate_ScalesWithTokens(t *testing.T) {
	r1 := EarningRate("rtx4090", 1000)
	r2 := EarningRate("rtx4090", 2000)
	if r2 != r1*2 {
		t.Fatalf("expected 2× scaling, got %f vs %f", r1, r2)
	}
	if EarningRate("rtx4090", 0) != 0 {
		t.Fatal("zero tokens should earn zero")
	}
}

func TestEarningRate_UnknownGPUFallsBackToCPU(t *testing.T) {
	got := EarningRate("nvidia-fake-9999", 1000)
	want := ComputeMineBaseRate * GPUMultiplierCPU
	if got != want {
		t.Fatalf("unknown gpu should fall back to CPU multiplier, got %f", got)
	}
}

// ─── validateNodeURL ─────────────────────────────

func TestValidateNodeURL(t *testing.T) {
	good := []string{"http://localhost:11434", "https://node.example.com", "http://10.0.0.5:8000"}
	bad := []string{"", "localhost:11434", "ftp://x", "://", "javascript:alert(1)"}
	for _, u := range good {
		if err := validateNodeURL(u); err != nil {
			t.Fatalf("expected %q to be valid: %v", u, err)
		}
	}
	for _, u := range bad {
		if err := validateNodeURL(u); !errors.Is(err, ErrInvalidNodeURL) {
			t.Fatalf("expected %q to be invalid, got %v", u, err)
		}
	}
}

// ─── RegisterNode ────────────────────────────────

func TestRegisterNode_RejectsInvalidURL(t *testing.T) {
	miner, _, _ := newMockMiner(t)
	_, err := miner.RegisterNode(context.Background(), InferenceNode{
		WorkspaceID: "ws",
		URL:         "not-a-url",
		Provider:    "ollama",
		GPUType:     "cpu",
	})
	if !errors.Is(err, ErrInvalidNodeURL) {
		t.Fatalf("expected ErrInvalidNodeURL, got %v", err)
	}
}

func TestRegisterNode_RejectsUnknownGPU(t *testing.T) {
	miner, _, _ := newMockMiner(t)
	_, err := miner.RegisterNode(context.Background(), InferenceNode{
		WorkspaceID: "ws",
		URL:         "http://x:1",
		Provider:    "ollama",
		GPUType:     "tpu",
	})
	if !errors.Is(err, ErrInvalidGPUType) {
		t.Fatalf("expected ErrInvalidGPUType, got %v", err)
	}
}

// ─── RecordServedRequest ─────────────────────────

// expectGetNode programmes the SELECT used by RecordServedRequest.
func expectGetNode(mock pgxmock.PgxPoolIface, n InferenceNode) {
	mock.ExpectQuery("SELECT id, workspace_id, url, provider").
		WithArgs(n.ID).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "provider", "models", "gpu_type",
			"max_concurrent", "price_per_token", "active", "verified", "created_at",
		}).AddRow(
			n.ID, n.WorkspaceID, n.URL, n.Provider, n.Models, n.GPUType,
			n.MaxConcurrent, n.PricePerToken, n.Active, n.Verified, n.CreatedAt,
		))
}

func TestRecordServedRequest_CreditsOwner(t *testing.T) {
	miner, _, mock := newMockMiner(t)

	node := InferenceNode{
		ID: "node1", WorkspaceID: "ws_owner", URL: "http://x", Provider: "ollama",
		Models: []string{"llama3"}, GPUType: "rtx4090", MaxConcurrent: 4,
		PricePerToken: 0.05, Active: true, Verified: true, CreatedAt: time.Now(),
	}
	expectGetNode(mock, node)
	// 2000 tokens × rtx4090 (1.0×) → 0.10 LENS.
	expectCreditOnce(mock, "req-1", "ws_owner", TypeComputeMine, 0, 0, 0, 0.10, 0.10, 0.10, 0)
	// metrics UPDATE
	mock.ExpectExec("UPDATE node_metrics").
		WithArgs("node1", 2000, int64(250)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := miner.RecordServedRequest(context.Background(),
		"node1", "ws_requester", "req-1", 2000, 250); err != nil {
		t.Fatalf("RecordServedRequest: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestRecordServedRequest_UsesGPUMultiplier(t *testing.T) {
	miner, _, mock := newMockMiner(t)
	node := InferenceNode{
		ID: "h100node", WorkspaceID: "ws_h", URL: "http://x", Provider: "vllm",
		Models: []string{"llama3"}, GPUType: "h100", MaxConcurrent: 4,
		PricePerToken: 0.15, Active: true, Verified: true, CreatedAt: time.Now(),
	}
	expectGetNode(mock, node)
	// 1000 tokens × h100 (3.0×) → 0.15 LENS.
	expectCreditOnce(mock, "req-1", "ws_h", TypeComputeMine, 0, 0, 0, 0.15, 0.15, 0.15, 0)
	mock.ExpectExec("UPDATE node_metrics").
		WithArgs("h100node", 1000, int64(180)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := miner.RecordServedRequest(context.Background(),
		"h100node", "ws_requester", "req-1", 1000, 180); err != nil {
		t.Fatalf("RecordServedRequest: %v", err)
	}
}

func TestRecordServedRequest_NoSelfServing(t *testing.T) {
	miner, _, mock := newMockMiner(t)
	node := InferenceNode{
		ID: "n2", WorkspaceID: "ws_self", URL: "http://x", Provider: "ollama",
		Models: []string{"llama3"}, GPUType: "cpu", MaxConcurrent: 1,
		PricePerToken: 0.025, Active: true, Verified: true, CreatedAt: time.Now(),
	}
	expectGetNode(mock, node)
	// No Credit expectation — only the metrics update (no ledger touch).
	mock.ExpectExec("UPDATE node_metrics").
		WithArgs("n2", 500, int64(120)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := miner.RecordServedRequest(context.Background(),
		"n2", "ws_self", "req-1", 500, 120); err != nil {
		t.Fatalf("RecordServedRequest: %v", err)
	}
}

func TestRecordServedRequest_ZeroTokensSkip(t *testing.T) {
	miner, _, _ := newMockMiner(t)
	// No expectations: must short-circuit without touching DB.
	if err := miner.RecordServedRequest(context.Background(), "node", "ws_x", "req-1", 0, 100); err != nil {
		t.Fatalf("RecordServedRequest: %v", err)
	}
}

// ─── ListAvailableNodes ──────────────────────────

func TestListAvailableNodes_FiltersByModel(t *testing.T) {
	miner, _, mock := newMockMiner(t)
	mock.ExpectQuery("WHERE verified = TRUE AND active = TRUE AND \\$1 = ANY\\(models\\)").
		WithArgs("llama3").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "provider", "models", "gpu_type",
			"max_concurrent", "price_per_token", "active", "verified", "created_at",
		}).
			AddRow("n1", "ws1", "http://a", "ollama", []string{"llama3"}, "rtx4090",
				4, 0.05, true, true, time.Now()).
			AddRow("n2", "ws2", "http://b", "ollama", []string{"llama3", "mistral"}, "a100",
				8, 0.07, true, true, time.Now()))
	nodes, err := miner.ListAvailableNodes(context.Background(), "llama3")
	if err != nil {
		t.Fatalf("ListAvailableNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 available nodes, got %d", len(nodes))
	}
	for _, n := range nodes {
		if !n.Verified || !n.Active {
			t.Fatalf("expected verified+active, got %+v", n)
		}
	}
}

// ─── GetNodeStats ────────────────────────────────

func TestGetNodeStats_ReturnsMetrics(t *testing.T) {
	miner, _, mock := newMockMiner(t)
	last := time.Now().UTC()
	mock.ExpectQuery("SELECT node_id, requests_served, tokens_served").
		WithArgs("n_stats").
		WillReturnRows(pgxmock.NewRows([]string{
			"node_id", "requests_served", "tokens_served",
			"avg_latency_ms", "error_rate", "uptime_pct", "last_active_at",
		}).AddRow("n_stats", 42, int64(10_000), int64(200), 0.01, 99.5, last))
	stats, err := miner.GetNodeStats(context.Background(), "n_stats")
	if err != nil {
		t.Fatalf("GetNodeStats: %v", err)
	}
	if stats.RequestsServed != 42 || stats.TokensServed != 10_000 {
		t.Fatalf("unexpected metrics: %+v", stats)
	}
	if stats.AvgLatencyMs != 200 || stats.UptimePct != 99.5 {
		t.Fatalf("unexpected metrics: %+v", stats)
	}
}

func TestGetNodeStats_UnknownNodeReturnsZero(t *testing.T) {
	miner, _, mock := newMockMiner(t)
	mock.ExpectQuery("SELECT node_id, requests_served, tokens_served").
		WithArgs("missing").
		WillReturnError(errPgxNoRows)
	stats, err := miner.GetNodeStats(context.Background(), "missing")
	if err != nil {
		t.Fatalf("GetNodeStats: %v", err)
	}
	if stats.NodeID != "missing" || stats.RequestsServed != 0 {
		t.Fatalf("expected zero stats, got %+v", stats)
	}
	if stats.UptimePct != 100 {
		t.Fatalf("expected uptime baseline 100, got %f", stats.UptimePct)
	}
}

// ─── DeactivateNode ──────────────────────────────

func TestDeactivateNode_NotFound(t *testing.T) {
	miner, _, mock := newMockMiner(t)
	mock.ExpectExec("UPDATE inference_nodes SET active = FALSE").
		WithArgs("ghost", "ws-x").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	err := miner.DeactivateNode(context.Background(), "ghost", "ws-x")
	if !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("expected ErrNodeNotFound, got %v", err)
	}
}
