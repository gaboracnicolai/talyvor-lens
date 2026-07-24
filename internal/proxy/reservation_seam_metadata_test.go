package proxy

import (
	"context"
	"encoding/json"
	"testing"
)

// TestSettleSeam_StampsRequestedAndServedModel proves the LIVE proxy path (agentReserveBlocks → settle)
// stamps the model on the spend row — closing the live-box gap where the settle's `spend` row read
// model=(null). requested_model comes from the hold (pre-route, the model the customer ASKED for);
// served_model is the routed model known at settle time. Asserted on the row, not a status code.
func TestSettleSeam_StampsRequestedAndServedModel(t *testing.T) {
	p, _, pool := seamProxy(t)
	ctx := context.Background()
	seamFund(t, pool, "ws", 100_000_000)

	// Hold on the REQUESTED model (gpt-4o), with a request id — both land on the reservation row.
	rctx, blocked := p.agentReserveBlocks(ctx, "agent", "ws", "gpt-4o", "a prompt of some length", "rq-seam", 4096)
	if blocked {
		t.Fatal("well-funded reserve must not block")
	}
	// Settle at the delivered cost, SERVED by the cheaper routed model.
	p.settleReservation(rctx, 0.012, "gpt-4o-mini")

	var metaStr string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(metadata::text, '{}') FROM lxc_ledger WHERE workspace_id='ws' AND type='spend' ORDER BY id DESC LIMIT 1`).
		Scan(&metaStr); err != nil {
		t.Fatalf("read spend row: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(metaStr), &meta); err != nil {
		t.Fatalf("spend metadata not JSON (%q): %v", metaStr, err)
	}
	if meta["requested_model"] != "gpt-4o" {
		t.Errorf("spend requested_model = %v, want gpt-4o (the model the customer ASKED for)", meta["requested_model"])
	}
	if meta["served_model"] != "gpt-4o-mini" {
		t.Errorf("spend served_model = %v, want gpt-4o-mini (the model that actually served)", meta["served_model"])
	}
	if meta["request_id"] != "rq-seam" {
		t.Errorf("spend request_id = %v, want rq-seam", meta["request_id"])
	}
}
