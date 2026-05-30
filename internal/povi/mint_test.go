package povi

import (
	"context"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/talyvor/lens/internal/metrics"
)

type fakeMinter struct {
	mu    sync.Mutex
	calls []creditCall
}

type creditCall struct {
	workspaceID string
	amount      float64
	txType      string
}

func (f *fakeMinter) Credit(_ context.Context, workspaceID string, amount float64, txType, _ string, _ map[string]interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, creditCall{workspaceID, amount, txType})
	return nil
}

// THE CRITICAL GATE: with the flag OFF (the default), minting does NOTHING —
// no ledger write, no provisional metric.
func TestMintFromReceipt_DisabledByDefault_NoLedgerWrite(t *testing.T) {
	m := &fakeMinter{}
	before := testutil.ToFloat64(metrics.POVIProvisionalMintsTotal)

	r := sampleReceipt()
	r.OutputTokens = 1000
	minted, amount, err := MintFromReceipt(context.Background(), m, r, false)
	if err != nil {
		t.Fatalf("MintFromReceipt: %v", err)
	}
	if minted {
		t.Error("minted=true with the flag off — minting must be gated")
	}
	if amount != 0 {
		t.Errorf("amount = %v, want 0 when disabled", amount)
	}
	if len(m.calls) != 0 {
		t.Errorf("ledger was credited %d times with the flag off — must be 0", len(m.calls))
	}
	if got := testutil.ToFloat64(metrics.POVIProvisionalMintsTotal); got != before {
		t.Errorf("provisional-mint metric moved while disabled: %v → %v", before, got)
	}
}

// With the flag ON (test-only / unsafe), a provisional mint records to the
// ledger and the provisional metric increments — proving the gate works and is
// observable when flipped.
func TestMintFromReceipt_EnabledRecordsProvisionalMint(t *testing.T) {
	m := &fakeMinter{}
	before := testutil.ToFloat64(metrics.POVIProvisionalMintsTotal)

	r := sampleReceipt()
	r.WorkspaceID = "ws-node-owner"
	r.OutputTokens = 1000
	minted, amount, err := MintFromReceipt(context.Background(), m, r, true)
	if err != nil {
		t.Fatalf("MintFromReceipt: %v", err)
	}
	if !minted || amount <= 0 {
		t.Fatalf("expected a provisional mint, got minted=%v amount=%v", minted, amount)
	}
	if len(m.calls) != 1 {
		t.Fatalf("expected exactly 1 ledger credit, got %d", len(m.calls))
	}
	c := m.calls[0]
	if c.workspaceID != "ws-node-owner" {
		t.Errorf("credited %q, want the node owner's workspace", c.workspaceID)
	}
	if c.amount != amount || c.txType != TypeReceiptMineProvisional {
		t.Errorf("credit = %+v, want amount=%v type=%q", c, amount, TypeReceiptMineProvisional)
	}
	if got := testutil.ToFloat64(metrics.POVIProvisionalMintsTotal); got != before+1 {
		t.Errorf("provisional-mint metric = %v, want %v", got, before+1)
	}
}

func TestProvisionalMintAmount_ScalesWithOutputTokens(t *testing.T) {
	r := sampleReceipt()
	r.OutputTokens = 2000
	if got, want := ProvisionalMintAmount(r), ReceiptMineRate*2.0; got != want {
		t.Errorf("amount = %v, want %v", got, want)
	}
}
