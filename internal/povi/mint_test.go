package povi

import (
	"context"
	"errors"
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
	amount      int64
	txType      string
}

func (f *fakeMinter) Credit(_ context.Context, workspaceID string, amount int64, txType, _ string, _ map[string]interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, creditCall{workspaceID, amount, txType})
	return nil
}

// fakeMeasure is Lens's gateway measurement store, faked. A request_id absent
// from the map returns (nil, nil) — Lens has NO record of that request.
type fakeMeasure struct{ byReq map[string]*ServedMeasurement }

func (f fakeMeasure) ServedMeasurement(_ context.Context, requestID string) (*ServedMeasurement, error) {
	return f.byReq[requestID], nil
}

func measuredAs(requestID, nodeID string, outputTokens int) fakeMeasure {
	return fakeMeasure{byReq: map[string]*ServedMeasurement{
		requestID: {NodeID: nodeID, OutputTokens: outputTokens},
	}}
}

// THE CRITICAL GATE: with the flag OFF (the default), minting does NOTHING —
// no ledger write, no provisional metric, and the measurement is never even
// consulted (nil lookup is fine).
func TestMintFromReceipt_DisabledByDefault_NoLedgerWrite(t *testing.T) {
	m := &fakeMinter{}
	before := testutil.ToFloat64(metrics.POVIProvisionalMintsTotal)

	r := sampleReceipt()
	r.OutputTokens = 1000
	minted, amount, err := MintFromReceipt(context.Background(), m, nil, r, false)
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

// THE MONEY PROPERTY: the mint is priced on what LENS MEASURED for the request,
// NEVER on the node's claimed OutputTokens. A receipt claiming 10,000,000 output
// tokens against a request Lens measured at 500 mints the amount for 500.
func TestMintFromReceipt_PricesOnGatewayMeasurement_NotClaim(t *testing.T) {
	m := &fakeMinter{}

	r := sampleReceipt()
	r.RequestID = "req-measured"
	r.NodeID = "node-A"
	r.WorkspaceID = "ws-node-owner"
	r.OutputTokens = 10_000_000 // the CLAIM — must not reach the amount

	measure := measuredAs("req-measured", "node-A", 500) // Lens MEASURED 500

	minted, amount, err := MintFromReceipt(context.Background(), m, measure, r, true)
	if err != nil {
		t.Fatalf("MintFromReceipt: %v", err)
	}
	if !minted {
		t.Fatal("a measured, node-bound receipt with minting ON must mint")
	}
	wantMeasured := ProvisionalMintAmountTokens(500)
	if amount != wantMeasured {
		t.Errorf("amount = %v, want %v (priced on MEASURED 500 tokens)", amount, wantMeasured)
	}
	if claimAmount := ProvisionalMintAmountTokens(r.OutputTokens); amount == claimAmount {
		t.Errorf("amount == %v == the CLAIM's price — the node's OutputTokens reached the amount", claimAmount)
	}
	if len(m.calls) != 1 || m.calls[0].amount != wantMeasured || m.calls[0].workspaceID != "ws-node-owner" {
		t.Errorf("ledger credit = %+v, want one credit of %v to ws-node-owner", m.calls, wantMeasured)
	}
}

// FAIL CLOSED: a receipt for a request_id Lens has NO measurement of mints
// NOTHING — no ledger write — and returns the distinct ErrNoServedMeasurement.
func TestMintFromReceipt_NoMeasurement_MintsNothing(t *testing.T) {
	m := &fakeMinter{}

	r := sampleReceipt()
	r.RequestID = "req-never-served"
	r.OutputTokens = 5000

	measure := fakeMeasure{byReq: map[string]*ServedMeasurement{}} // Lens served nothing

	minted, amount, err := MintFromReceipt(context.Background(), m, measure, r, true)
	if !errors.Is(err, ErrNoServedMeasurement) {
		t.Fatalf("err = %v, want ErrNoServedMeasurement", err)
	}
	if minted || amount != 0 {
		t.Errorf("minted=%v amount=%v, want false/0 — no measurement means no mint", minted, amount)
	}
	if len(m.calls) != 0 {
		t.Errorf("ledger credited %d times for an unmeasured request — must be 0", len(m.calls))
	}
}

// BINDING: a receipt whose node did NOT serve the measured request (the
// measurement is bound to a different node) mints NOTHING — a node cannot name
// another node's request. Returns the distinct ErrMeasurementNodeMismatch.
func TestMintFromReceipt_NodeMismatch_MintsNothing(t *testing.T) {
	m := &fakeMinter{}

	r := sampleReceipt()
	r.RequestID = "req-served-by-B"
	r.NodeID = "node-A" // the receipt claims node-A served it
	r.OutputTokens = 500

	measure := measuredAs("req-served-by-B", "node-B", 500) // but Lens dispatched it to node-B

	minted, amount, err := MintFromReceipt(context.Background(), m, measure, r, true)
	if !errors.Is(err, ErrMeasurementNodeMismatch) {
		t.Fatalf("err = %v, want ErrMeasurementNodeMismatch", err)
	}
	if minted || amount != 0 {
		t.Errorf("minted=%v amount=%v, want false/0 — node did not serve this request", minted, amount)
	}
	if len(m.calls) != 0 {
		t.Errorf("ledger credited %d times across a node boundary — must be 0", len(m.calls))
	}
}

// The provisional-mint metric increments only on an ACTUAL measured mint, so
// flipping the flag stays observable.
func TestMintFromReceipt_EnabledRecordsProvisionalMint(t *testing.T) {
	m := &fakeMinter{}
	before := testutil.ToFloat64(metrics.POVIProvisionalMintsTotal)

	r := sampleReceipt()
	r.RequestID = "req-obs"
	r.NodeID = "node-obs"
	r.WorkspaceID = "ws-node-owner"
	r.OutputTokens = 1000
	measure := measuredAs("req-obs", "node-obs", 1000)

	minted, amount, err := MintFromReceipt(context.Background(), m, measure, r, true)
	if err != nil {
		t.Fatalf("MintFromReceipt: %v", err)
	}
	if !minted || amount <= 0 {
		t.Fatalf("expected a provisional mint, got minted=%v amount=%v", minted, amount)
	}
	if c := m.calls[0]; c.amount != amount || c.txType != TypeReceiptMineProvisional {
		t.Errorf("credit = %+v, want amount=%v type=%q", c, amount, TypeReceiptMineProvisional)
	}
	if got := testutil.ToFloat64(metrics.POVIProvisionalMintsTotal); got != before+1 {
		t.Errorf("provisional-mint metric = %v, want %v", got, before+1)
	}
}

func TestProvisionalMintAmountTokens_ScalesWithMeasuredTokens(t *testing.T) {
	if got, want := ProvisionalMintAmountTokens(2000), ReceiptMineRate*2; got != want {
		t.Errorf("amount = %v, want %v", got, want)
	}
}
