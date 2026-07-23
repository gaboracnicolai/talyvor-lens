package economy

import (
	"context"
	"testing"
)

// RED-FIRST on the ROW. The agent-allocator debit used to write metadata=nil, so
// every lxc_ledger row read identically ("proof-of-agent-allocation: pre-serve
// estimate debit") and per-model spend was not derivable. The debit must now
// stamp the REQUESTED model (the charge is estimated pre-routing) and the
// token_events request_id (so a money row joins to its usage row) onto the row's
// metadata — WITHOUT disturbing the money columns, and WITHOUT ever carrying
// content (lxc_ledger is append-only + immutable, migration 0055).

// (row proof) the metadata lands AND the money columns are untouched.
func TestAgentSpend_StampsRequestedModelAndRequestID(t *testing.T) {
	s := agentHarness(t)
	ctx := context.Background()
	fund(t, s, "wsM", 100*uLXC)

	const model = "claude-sonnet-5"    // the REQUESTED model — the debit precedes routing
	const reqID = "req-tok-abc123"     // the token_events request_id, for the join
	const desc = "proof-of-agent-allocation: pre-serve estimate debit"
	if err := s.SpendLXCForAgent(ctx, "keyM", "wsM", "debit-key-1", 10*uLXC, desc,
		AgentDebitMeta{RequestedModel: model, RequestID: reqID}); err != nil {
		t.Fatalf("spend: %v", err)
	}

	var (
		gotModel, gotReqID, gotType, gotDesc string
		gotAmount, gotBalanceAfter           int64
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(metadata->>'requested_model',''), COALESCE(metadata->>'request_id',''), type, description, amount, balance_after
		FROM lxc_ledger WHERE workspace_id='wsM' AND type='spend'`).
		Scan(&gotModel, &gotReqID, &gotType, &gotDesc, &gotAmount, &gotBalanceAfter); err != nil {
		t.Fatalf("read row: %v", err)
	}

	// (a) the metadata LANDS — the whole point of the fix.
	if gotModel != model {
		t.Errorf("metadata.requested_model = %q, want %q", gotModel, model)
	}
	if gotReqID != reqID {
		t.Errorf("metadata.request_id = %q, want %q", gotReqID, reqID)
	}
	// (b) the MONEY COLUMNS are untouched by the metadata addition (label≠amount trap).
	if gotAmount != -10*uLXC {
		t.Errorf("amount = %d, want %d (a debit; metadata must not disturb the money column)", gotAmount, -10*uLXC)
	}
	if gotBalanceAfter != 90*uLXC {
		t.Errorf("balance_after = %d, want %d", gotBalanceAfter, 90*uLXC)
	}
	if gotType != LXCTypeSpend {
		t.Errorf("type = %q, want %q", gotType, LXCTypeSpend)
	}
	if gotDesc != desc {
		t.Errorf("description = %q, want unchanged %q", gotDesc, desc)
	}
}

// (invariant) AgentDebitMeta carries ONLY the two allowed scalar keys — a
// financial record can never become a content record. Pure unit; no PG.
func TestAgentDebitMeta_CarriesOnlyTwoScalarKeys(t *testing.T) {
	m := AgentDebitMeta{RequestedModel: "m", RequestID: "r"}.toMap()
	if len(m) != 2 {
		t.Fatalf("metadata must carry exactly two keys, got %d: %v", len(m), m)
	}
	if _, ok := m["requested_model"]; !ok {
		t.Error("missing requested_model key")
	}
	if _, ok := m["request_id"]; !ok {
		t.Error("missing request_id key")
	}
	// empty scalars omit their key — no empty noise on the money row.
	if got := len(AgentDebitMeta{}.toMap()); got != 0 {
		t.Errorf("empty meta must produce no keys, got %d", got)
	}
}
