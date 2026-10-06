package economy

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/talyvor/lens/internal/metrics"
)

// B35.3 — a settle whose delivered cost is above its hold still charges only the hold, but never silently: an
// ERROR log with the reservation, the model, the delivered cost and the hold, lens_agent_hold_cuts_total, and
// the written-off part on the spend row. A settle under its hold writes none of the three.
func TestB353_ASettleCutToItsHoldIsLoggedCountedAndWrittenOffOnTheRow(t *testing.T) {
	s := reservationHarness(t)
	ctx := context.Background()
	resFund(t, s, "ws", 1_000_000)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	cuts := metrics.AgentHoldCutsTotal.WithLabelValues("claude-haiku-4-5")
	before := testutil.ToFloat64(cuts)

	for _, id := range []string{"res-cut", "res-fits"} {
		if err := s.ReserveLXCForAgent(ctx, "agent", "ws", id, 971,
			AgentDebitMeta{RequestedModel: "claude-haiku-4-5", RequestID: "rq-" + id}); err != nil {
			t.Fatalf("reserve %s: %v", id, err)
		}
	}
	// The control: delivered under the hold, charged exactly that, nothing written off.
	if charged, _, err := s.SettleLXCReservation(ctx, "res-fits", 540, AgentDebitMeta{ServedModel: "claude-haiku-4-5"}); err != nil || charged != 540 {
		t.Fatalf("a settle under its hold charged %d (%v), want 540", charged, err)
	}
	if logs.Len() != 0 || testutil.ToFloat64(cuts) != before {
		t.Fatalf("a settle under its hold logged %q and moved the cut metric", logs.String())
	}
	// The cut: 4,430 delivered against a hold of 971.
	charged, _, err := s.SettleLXCReservation(ctx, "res-cut", 4_430, AgentDebitMeta{ServedModel: "claude-haiku-4-5"})
	if err != nil {
		t.Fatal(err)
	}
	if charged != 971 {
		t.Fatalf("charged %d, want the hold, 971", charged)
	}
	if got := testutil.ToFloat64(cuts) - before; got != 1 {
		t.Errorf("lens_agent_hold_cuts_total{model=claude-haiku-4-5} rose by %v, want 1", got)
	}
	line := logs.String()
	for _, want := range []string{"level=ERROR", "reservation=res-cut", "model=claude-haiku-4-5", "delivered_ulxc=4430",
		"held_ulxc=971", "written_off_ulxc=3459"} {
		if !strings.Contains(line, want) {
			t.Errorf("the cut logged %q, missing %q", line, want)
		}
	}
	var spends []metaLedgerRow
	for _, r := range resLedgerMeta(t, s, "ws") {
		if r.typ == LXCTypeSpend {
			spends = append(spends, r)
		}
	}
	if len(spends) != 2 {
		t.Fatalf("%d spend rows, want 2", len(spends))
	}
	if _, ok := spends[0].meta["written_off_ulxc"]; ok {
		t.Errorf("the settle under its hold carries written_off_ulxc: %v", spends[0].meta)
	}
	if spends[1].amount != -971 || spends[1].meta["written_off_ulxc"] != float64(3459) {
		t.Errorf("the cut's spend row is %d with %v, want -971 with written_off_ulxc 3459", spends[1].amount, spends[1].meta)
	}
}
