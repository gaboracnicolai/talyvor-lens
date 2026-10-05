package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/market"
)

type fakeJournal map[string]market.JournalReconciliation

func (f fakeJournal) JournalSellers(context.Context) ([]string, error) {
	return []string{"ws-a", "ws-b"}, nil
}
func (f fakeJournal) JournalCheck(_ context.Context, ws string) (market.JournalReconciliation, error) {
	return f[ws], nil
}

// B32.17 — `lens market journal-check` prints every seller, says why one does not reconcile, and then fails, so
// the operator (or a cron) sees it.
func TestMarketJournalCheck_NamesTheSellerThatDoesNotReconcileAndFails(t *testing.T) {
	store := fakeJournal{
		"ws-a": {WorkspaceID: "ws-a", InHoldbackUSDMicros: 8_500_000, AvailableUSDMicros: -1_234_567},
		"ws-b": {WorkspaceID: "ws-b", Mismatches: []string{"the journal holds 1 µUSD available; the earnings less the payouts say 0"}},
	}
	var out bytes.Buffer
	err := marketCommand(context.Background(), store, []string{"journal-check"}, &out)
	if err == nil || !strings.Contains(err.Error(), "1 of 2 sellers do not reconcile") {
		t.Fatalf("journal-check = %v; want it to fail naming 1 of 2 sellers", err)
	}
	for _, want := range []string{
		"ws-a\tok\tholdback $8.50\tavailable -$1.234567",
		"ws-b\tDOES NOT RECONCILE\tthe journal holds 1 µUSD available",
		"2 sellers checked, 1 do not reconcile",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("journal-check printed:\n%s\nwant a line with %q", out.String(), want)
		}
	}
	out.Reset()
	if err := marketCommand(context.Background(), store, []string{"journal-check", "ws-a"}, &out); err != nil {
		t.Fatalf("journal-check ws-a = %v; one seller that reconciles passes", err)
	}
}
