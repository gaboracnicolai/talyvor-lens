package economy

import (
	"context"
	"encoding/json"
	"testing"
)

// THE POOLED-HIT DISCOUNT, asserted on LEDGER ROWS.
//
// A cross-tenant pooled hit used to charge the consumer the FULL avoided provider cost. Live
// numbers: workspace A paid 640 µLXC upstream, workspace B took the pooled hit and paid 920 (a
// longer cached answer — the arithmetic was exact, not a bug). B saved NOTHING, and the first
// tester's reaction was "why did the cache cost me the same?".
//
// A cache hit costs Talyvor nothing — there is no provider call — so the whole charge is margin
// split between the contributor and us. The discount hands part of it back.
//
// ⚠ WHY THE LEDGER AND NOT A RETURN VALUE. The consumer's evidence is the row, not a float. A
// function returning the right number while the row records the old one is exactly the failure
// this must exclude, and it is invisible to any test that asserts the return.
//
// ⚠ WHY `saved` IS DERIVED INSIDE THE SETTLE. The settle CLAMPS the charge to the hold
// (finalLXC > heldLXC ⇒ finalLXC = heldLXC), so a saving computed by the caller and passed
// alongside can disagree with the amount actually debited — two sources of truth for one fact.
// The caller passes the LIST price and the rate; the row derives saved = list − what was really
// charged. The three numbers then reconcile by construction, on every row, including clamped ones.

// poolMeta reads the metadata document off the delivered-charge (spend) row.
func poolMeta(t *testing.T, s *DualTokenStore, ws string) (amount int64, meta map[string]any) {
	t.Helper()
	var raw []byte
	err := s.pool.QueryRow(context.Background(),
		`SELECT amount, metadata FROM lxc_ledger
		  WHERE workspace_id = $1 AND type = $2 ORDER BY id DESC LIMIT 1`, ws, LXCTypeSpend).
		Scan(&amount, &raw)
	if err != nil {
		t.Fatalf("no delivered-charge row for %s: %v", ws, err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("metadata is not a JSON document: %v", err)
	}
	return amount, meta
}

func metaInt(t *testing.T, m map[string]any, k string) int64 {
	t.Helper()
	v, ok := m[k]
	if !ok {
		t.Fatalf("metadata has no %q — the consumer cannot see the saving, which is the whole point: "+
			"a charge of 644 with nothing saying it would have been 920 is indistinguishable from a "+
			"cheaper answer. keys present: %v", k, keysOfAny(m))
	}
	f, ok := v.(float64) // encoding/json decodes every number as float64
	if !ok {
		t.Fatalf("metadata[%q] = %T, want a number", k, v)
	}
	return int64(f)
}

func keysOfAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ⚠ THE USER-VISIBLE CLAIM: the pooled hit is CHARGED LESS than the same request upstream, and the
// row says by how much. The live numbers are used verbatim so the regression reads as the incident.
func TestPoolDiscount_LedgerRowCarriesListAndSaving(t *testing.T) {
	s := reservationHarness(t)
	ctx := context.Background()
	const ws, key = "ws-consumer", "key-consumer"

	resFund(t, s, ws, 5_000_000)
	// The hold is the conservative pre-serve estimate; the list price is what the live call would
	// have cost (920 µLXC — the incident's number).
	if err := s.ReserveLXCForAgent(ctx, key, ws, "res-pooled-1", 2_000_000,
		AgentDebitMeta{RequestedModel: "gpt-5.3", RequestID: "req-pooled-1"}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 30% off 920 = 644, the number the consumer should actually be billed.
	settled, err := s.SettleLXCReservation(ctx, "res-pooled-1", 644, AgentDebitMeta{
		ServedModel: "gpt-5.3", PoolListULXC: 920, PoolDiscountRate: 0.30,
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if settled != 644 {
		t.Fatalf("settled = %d µLXC, want 644 — the consumer must be charged the DISCOUNTED price", settled)
	}

	amount, meta := poolMeta(t, s, ws)
	if amount != -644 {
		t.Errorf("ledger amount = %d, want -644", amount)
	}
	if got := metaInt(t, meta, "pool_list_ulxc"); got != 920 {
		t.Errorf("pool_list_ulxc = %d, want 920 (what it WOULD have cost)", got)
	}
	if got := metaInt(t, meta, "pool_saved_ulxc"); got != 276 {
		t.Errorf("pool_saved_ulxc = %d, want 276", got)
	}
	if r, ok := meta["pool_discount_rate"].(float64); !ok || r != 0.30 {
		t.Errorf("pool_discount_rate = %v, want 0.30 — the rate must be on the row so a bill can be "+
			"audited against the rate in force WHEN IT WAS CHARGED, not the rate configured today", meta["pool_discount_rate"])
	}
	// The three numbers must reconcile, or the row tells two stories.
	if list, saved := metaInt(t, meta, "pool_list_ulxc"), metaInt(t, meta, "pool_saved_ulxc"); saved+(-amount) != list {
		t.Errorf("charged(%d) + saved(%d) != list(%d) — the row does not reconcile", -amount, saved, list)
	}
}

// ⚠ THE CLAMPED CASE, which is where a passed-in saving would start lying. The settle refuses to
// bill above the hold, so the ACTUAL charge can be below the discounted price. `saved` must be
// derived from what was really debited — otherwise the row claims a saving the customer did not get
// (and the arithmetic silently stops adding up).
func TestPoolDiscount_SavingIsDerivedFromWhatWasActuallyCharged(t *testing.T) {
	s := reservationHarness(t)
	ctx := context.Background()
	const ws, key = "ws-clamped", "key-clamped"

	resFund(t, s, ws, 5_000_000)
	// Hold is only 500, below the 644 discounted price: the settle clamps to 500.
	if err := s.ReserveLXCForAgent(ctx, key, ws, "res-pooled-2", 500,
		AgentDebitMeta{RequestedModel: "gpt-5.3", RequestID: "req-pooled-2"}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	settled, err := s.SettleLXCReservation(ctx, "res-pooled-2", 644, AgentDebitMeta{
		ServedModel: "gpt-5.3", PoolListULXC: 920, PoolDiscountRate: 0.30,
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if settled != 500 {
		t.Fatalf("settled = %d, want 500 (clamped to the hold)", settled)
	}
	amount, meta := poolMeta(t, s, ws)
	if saved := metaInt(t, meta, "pool_saved_ulxc"); saved != 420 {
		t.Errorf("pool_saved_ulxc = %d, want 420 (920 − 500 actually charged). A saving passed in "+
			"alongside would still read 276 here and the row would not reconcile.", saved)
	}
	if list, saved := metaInt(t, meta, "pool_list_ulxc"), metaInt(t, meta, "pool_saved_ulxc"); saved+(-amount) != list {
		t.Errorf("charged(%d) + saved(%d) != list(%d)", -amount, saved, list)
	}
}

// An ORDINARY (non-pooled) charge must be completely unchanged: no pool keys, no noise on the row.
// Own-cache hits are already free and are not settled at all — this covers the upstream miss path.
func TestPoolDiscount_OrdinaryChargeRowIsUntouched(t *testing.T) {
	s := reservationHarness(t)
	ctx := context.Background()
	const ws, key = "ws-plain", "key-plain"

	resFund(t, s, ws, 5_000_000)
	if err := s.ReserveLXCForAgent(ctx, key, ws, "res-plain", 2_000_000,
		AgentDebitMeta{RequestedModel: "gpt-5.3", RequestID: "req-plain"}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.SettleLXCReservation(ctx, "res-plain", 640, AgentDebitMeta{ServedModel: "gpt-5.3"}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	_, meta := poolMeta(t, s, ws)
	for _, k := range []string{"pool_list_ulxc", "pool_saved_ulxc", "pool_discount_rate"} {
		if _, ok := meta[k]; ok {
			t.Errorf("an ordinary upstream charge carries %q — a row that claims a pooled saving on a "+
				"live provider call would overstate the discount's reach in any bill built from this ledger", k)
		}
	}
}
