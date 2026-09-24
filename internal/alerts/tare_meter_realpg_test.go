package alerts

import (
	"context"
	"testing"
)

// B6.4 DONE: a query against the new columns returns tokens in, tokens out and delta cost for a
// request, tagged with its work item — through the real writer and the real reader, on the real
// schema. A spend row Tare did not touch must not appear in the savings at all.
func TestTareMeter_WriterAndReaderOnTheRealSchema(t *testing.T) {
	pool := cacheServePool(t)
	ctx := context.Background()
	const ws, model = "ws-tare-meter", "claude-haiku-4-5"
	if _, err := pool.Exec(ctx, `DELETE FROM token_events WHERE workspace_id = $1`, ws); err != nil {
		t.Fatalf("reset: %v", err)
	}
	a := New(pool, nil, nil)

	meter := TareMeter{Kind: "json", TokensIn: 1200, TokensOut: 450, WorkItemID: "TALYVOR-123"}
	if err := a.RecordSpendWithTare(ctx, ws, "", "", "", model, 460, 20, "", "", "req-tare-1", "text", false, "", meter); err != nil {
		t.Fatalf("RecordSpendWithTare: %v", err)
	}
	if err := a.RecordSpend(ctx, ws, "", "", "", model, 900, 20, "", "", "req-plain-1", "text", false); err != nil {
		t.Fatalf("RecordSpend: %v", err)
	}

	got, err := a.TareSavings(ctx, ws)
	if err != nil {
		t.Fatalf("TareSavings: %v", err)
	}
	wantDelta := CostUSD(model, 1200-450, 0)
	if wantDelta <= 0 {
		t.Fatalf("%s has no catalog price — the delta below would be vacuous", model)
	}
	if len(got) != 1 {
		t.Fatalf("savings rows = %+v, want exactly the one Tare-reduced request", got)
	}
	s := got[0]
	if s.WorkItemID != "TALYVOR-123" || s.Requests != 1 || s.TokensIn != 1200 || s.TokensOut != 450 || s.DeltaCostUSD != wantDelta {
		t.Errorf("saving = %+v, want TALYVOR-123 · 1 request · 1200 → 450 tokens · $%.8f", s, wantDelta)
	}
}
