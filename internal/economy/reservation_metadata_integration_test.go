package economy

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Real-PG proofs that the reservation SETTLE and RELEASE (and the stranded sweeper) stamp the model +
// request_id on their lxc_ledger rows — a self-describing financial record — WITHOUT touching the money
// columns (amount / type / balance_after byte-identical to today). requested_model + request_id come from
// the reservation ROW (the single source the hold wrote), so every row matches the hold; served_model is
// the post-route model, stamped on the delivered-charge spend row only.

type metaLedgerRow struct {
	amount       int64
	typ          string
	balanceAfter int64
	meta         map[string]any
}

func resLedgerMeta(t *testing.T, s *DualTokenStore, ws string) []metaLedgerRow {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT amount, type, balance_after, COALESCE(metadata::text, '{}') FROM lxc_ledger WHERE workspace_id = $1 ORDER BY id`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []metaLedgerRow
	for rows.Next() {
		var r metaLedgerRow
		var metaStr string
		if err := rows.Scan(&r.amount, &r.typ, &r.balanceAfter, &metaStr); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(metaStr), &r.meta); err != nil {
			t.Fatalf("metadata not valid JSON (%q): %v", metaStr, err)
		}
		out = append(out, r)
	}
	return out
}

func rowByType(t *testing.T, rows []metaLedgerRow, typ string) metaLedgerRow {
	t.Helper()
	for _, r := range rows {
		if r.typ == typ {
			return r
		}
	}
	t.Fatalf("no ledger row of type %q in %+v", typ, rows)
	return metaLedgerRow{}
}

// TestSettleStampsRequestedAndServedModel: a settle's DELIVERED-charge spend row carries requested_model +
// request_id (from the reservation row, matching the hold) AND served_model (the post-route model); the
// settle's release row carries requested_model + request_id but NOT served_model (it is a refund, nothing
// served). Money columns are byte-identical to today.
func TestSettleStampsRequestedAndServedModel(t *testing.T) {
	s := reservationHarness(t)
	ctx := context.Background()
	resFund(t, s, "ws", 1_000_000)
	if err := s.ReserveLXCForAgent(ctx, "agent", "ws", "res1", 300_000,
		AgentDebitMeta{RequestedModel: "gpt-4o", RequestID: "rq1"}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Served by a cheaper routed model than the customer requested — the whole point of billing delivered.
	if _, err := s.SettleLXCReservation(ctx, "res1", 120_000, AgentDebitMeta{ServedModel: "gpt-4o-mini"}); err != nil {
		t.Fatalf("settle: %v", err)
	}

	rows := resLedgerMeta(t, s, "ws")
	hold := rowByType(t, rows, LXCTypeReservationHold)
	rel := rowByType(t, rows, LXCTypeReservationRelease)
	spend := rowByType(t, rows, LXCTypeSpend)

	// (A) MONEY byte-identical to today — the metadata fix must not move a single number.
	if hold.amount != -300_000 || hold.balanceAfter != 700_000 {
		t.Errorf("hold money changed: amount=%d balance_after=%d, want -300000 / 700000", hold.amount, hold.balanceAfter)
	}
	if rel.amount != 300_000 || rel.balanceAfter != 1_000_000 {
		t.Errorf("release money changed: amount=%d balance_after=%d, want 300000 / 1000000", rel.amount, rel.balanceAfter)
	}
	if spend.amount != -120_000 || spend.balanceAfter != 880_000 {
		t.Errorf("spend money changed: amount=%d balance_after=%d, want -120000 / 880000", spend.amount, spend.balanceAfter)
	}

	// (B) The SPEND row (the real charge) is self-describing: requested + served + request_id.
	if spend.meta["requested_model"] != "gpt-4o" {
		t.Errorf("spend requested_model = %v, want gpt-4o", spend.meta["requested_model"])
	}
	if spend.meta["served_model"] != "gpt-4o-mini" {
		t.Errorf("spend served_model = %v, want gpt-4o-mini", spend.meta["served_model"])
	}
	if spend.meta["request_id"] != "rq1" {
		t.Errorf("spend request_id = %v, want rq1", spend.meta["request_id"])
	}

	// (C) The RELEASE row carries requested_model + request_id, but NOT served_model (a refund served nothing).
	if rel.meta["requested_model"] != "gpt-4o" {
		t.Errorf("release requested_model = %v, want gpt-4o", rel.meta["requested_model"])
	}
	if rel.meta["request_id"] != "rq1" {
		t.Errorf("release request_id = %v, want rq1", rel.meta["request_id"])
	}
	if _, present := rel.meta["served_model"]; present {
		t.Errorf("release row must NOT carry served_model (nothing served on a refund): %v", rel.meta)
	}
}

// TestReleaseStampsRequestedModel: an in-band full RELEASE (cache hit / no delivery) stamps requested_model
// + request_id on its release row (previously NULL metadata), money unchanged.
func TestReleaseStampsRequestedModel(t *testing.T) {
	s := reservationHarness(t)
	ctx := context.Background()
	resFund(t, s, "ws", 1_000_000)
	if err := s.ReserveLXCForAgent(ctx, "agent", "ws", "res1", 300_000,
		AgentDebitMeta{RequestedModel: "claude-3-5-sonnet-20241022", RequestID: "rq2"}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := s.ReleaseLXCReservation(ctx, "res1", "cache hit"); err != nil {
		t.Fatalf("release: %v", err)
	}
	rel := rowByType(t, resLedgerMeta(t, s, "ws"), LXCTypeReservationRelease)
	if rel.amount != 300_000 || rel.balanceAfter != 1_000_000 {
		t.Errorf("release money changed: amount=%d balance_after=%d, want 300000 / 1000000", rel.amount, rel.balanceAfter)
	}
	if rel.meta["requested_model"] != "claude-3-5-sonnet-20241022" {
		t.Errorf("release requested_model = %v, want claude-3-5-sonnet-20241022", rel.meta["requested_model"])
	}
	if rel.meta["request_id"] != "rq2" {
		t.Errorf("release request_id = %v, want rq2", rel.meta["request_id"])
	}
}

// TestStrandedSweepStampsRequestedModel: the crash-recovery sweeper releases a stranded hold through the
// SAME ReleaseLXCReservation, so its release row also carries requested_model + request_id from the row.
func TestStrandedSweepStampsRequestedModel(t *testing.T) {
	s := reservationHarness(t)
	ctx := context.Background()
	resFund(t, s, "ws", 1_000_000)
	if err := s.ReserveLXCForAgent(ctx, "agent", "ws", "res1", 300_000,
		AgentDebitMeta{RequestedModel: "gpt-4o", RequestID: "rq3"}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE lxc_reservations SET created_at = now() - interval '1 hour' WHERE reservation_id = 'res1'`); err != nil {
		t.Fatal(err)
	}
	n, err := s.ReleaseStrandedReservations(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	rel := rowByType(t, resLedgerMeta(t, s, "ws"), LXCTypeReservationRelease)
	if rel.meta["requested_model"] != "gpt-4o" {
		t.Errorf("swept release requested_model = %v, want gpt-4o", rel.meta["requested_model"])
	}
	if rel.meta["request_id"] != "rq3" {
		t.Errorf("swept release request_id = %v, want rq3", rel.meta["request_id"])
	}
}
