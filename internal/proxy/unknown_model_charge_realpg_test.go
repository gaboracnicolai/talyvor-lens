package proxy

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/catalog"
)

// ledgerRow is one lxc_ledger row with its metadata document, so an assertion can read BOTH the money
// column and the basis marking. reservationLedgerRows (stream_settle_realpg_test.go) sums by type and
// drops metadata, which is the wrong shape for asserting a marking.
type ledgerRow struct {
	amount int64
	typ    string
	meta   map[string]any
}

func seamLedgerRows(t *testing.T, pool *pgxpool.Pool, ws string) []ledgerRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT amount, type, COALESCE(metadata::text, '{}') FROM lxc_ledger WHERE workspace_id=$1 ORDER BY id`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		var raw string
		if err := rows.Scan(&r.amount, &r.typ, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &r.meta); err != nil {
			t.Fatalf("metadata not JSON (%q): %v", raw, err)
		}
		out = append(out, r)
	}
	return out
}

func findRowByType(t *testing.T, rows []ledgerRow, typ string) ledgerRow {
	t.Helper()
	for _, r := range rows {
		if r.typ == typ {
			return r
		}
	}
	t.Fatalf("no %q row in the ledger: %+v", typ, rows)
	return ledgerRow{}
}

// UNKNOWN MODELS MUST NOT BE FREE.
//
// ── WHAT WAS ACTUALLY HAPPENING ──────────────────────────────────────────────
//
// catalog.Price returns ok=false for a model the catalog does not know, and the pricing helpers turned
// that "not found" into the VALUE ZERO. The whole chain, verified on main:
//
//	catalog.Price("claude-opus-5")            → ok=false
//	alerts.CostUSD / CostUSDDetailed          → 0        (`if !ok { return 0 }`)
//	reserveEstimateLXC                        → 0
//	agentReserveBlocks: heldLXC <= 0          → returns (ctx, false) — NO HOLD, NO HANDLE
//	settleReservation: reservationFrom(ctx) !ok → returns 0 — NO RELEASE ROW, NO SPEND ROW
//
// So a request on a model the provider serves and the catalog does not know: succeeds, costs the
// customer nothing, writes NO lxc_ledger row at all (not even a zero one — it is invisible, not just
// free), bypasses the agent sub-budget ceiling entirely because no reservation is ever taken, and
// leaves Talyvor holding the provider bill. token_events.cost_usd is 0 too, so every SUM(cost_usd)
// reader — budgets, forecast, anomaly detection, the ROI report — sees nothing.
//
// The origin is visible in alerts.go's own comment: "Unknown models cost 0 — we'd rather miss an alert
// than fire a false one off bad data." That was sound reasoning for an ALERTING package. The same
// function later became the BILLING basis, and the rationale did not travel with the reuse.
//
// ── THE ASSERTIONS ───────────────────────────────────────────────────────────
//
// These assert the LEDGER, not a return value: a served request on an unknown model must leave a
// non-zero spend row. A test that only checked a returned float could pass while the row was absent.

// unknownModel is a model id the catalog cannot know — deliberately not a real one, so this test
// keeps its teeth on the day the catalog learns about Opus 5.
const unknownModel = "provider-model-the-catalog-has-never-heard-of-v9"

// Pre-flight: the model really is unknown, and the OLD pricing helpers really do return zero for it.
// If either stops being true the tests below would pass for the wrong reason.
func TestUnknownModel_PreflightIsGenuinelyUnknown(t *testing.T) {
	if _, _, ok := catalog.Price(unknownModel); ok {
		t.Fatalf("%q is in the catalog — pick an id the catalog cannot know", unknownModel)
	}
	if got := alerts.CostUSD(unknownModel, 1_000_000, 1_000_000); got != 0 {
		t.Fatalf("CostUSD on an unknown model = %v, want 0 — the premise of this test file has changed", got)
	}
}

// ⭐ THE MONEY ASSERTION: a served request on an unknown model leaves a NON-ZERO spend row.
func TestUnknownModel_ServedRequestWritesNonZeroSpendRow(t *testing.T) {
	p, store, pool := seamProxy(t)
	ctx := context.Background()
	seamFund(t, pool, "ws", 100_000_000)

	rctx, blocked := p.agentReserveBlocks(ctx, "agent", "ws", unknownModel, "a prompt of some length", "rq-unknown", 4096)
	if blocked {
		t.Fatal("a funded workspace must not be blocked on an unknown model")
	}
	// A hold MUST have been taken: without one there is no reservation to settle, so the charge can
	// never happen no matter what the settle does.
	held := seamBalance(t, store, "ws")
	if held >= 100_000_000 {
		t.Fatalf("no hold was taken for an unknown model (balance still %d) — the request would settle to nothing", held)
	}

	// Serve it. This is the SAME call proxy.go makes to price the delivered charge.
	served, prov := alerts.CostUSDResolved(unknownModel, catalog.PurposeCharge, 2_000, 0, 0, 1_000)
	if served <= 0 {
		t.Fatalf("the served cost for an unknown model is %v — it must never be zero", served)
	}
	if prov != catalog.ProvenanceFallback {
		t.Fatalf("provenance = %v, want fallback for an unknown model", prov)
	}
	p.settleReservationBasis(rctx, served, unknownModel, prov.String())

	// THE LEDGER, not a return value.
	rows := seamLedgerRows(t, pool, "ws")
	spend := findRowByType(t, rows, "spend")
	if spend.amount >= 0 {
		t.Fatalf("spend row amount = %d, want a negative (debiting) amount", spend.amount)
	}
	if b := seamBalance(t, store, "ws"); b >= 100_000_000 {
		t.Fatalf("balance after settle = %d, want less than the starting 100000000 — the request was free", b)
	}
}

// The fallback must be MARKED on the row, so a bill built from the ledger can distinguish a real price
// from a guessed one. An unmarked fallback charge is a number nobody can audit later.
func TestUnknownModel_SpendRowMarksTheFallbackBasis(t *testing.T) {
	p, store, pool := seamProxy(t)
	ctx := context.Background()
	seamFund(t, pool, "ws", 100_000_000)
	rctx, _ := p.agentReserveBlocks(ctx, "agent", "ws", unknownModel, "prompt", "rq-mark", 4096)
	_ = store
	served, prov := alerts.CostUSDResolved(unknownModel, catalog.PurposeCharge, 2_000, 0, 0, 1_000)
	p.settleReservationBasis(rctx, served, unknownModel, prov.String())

	spend := findRowByType(t, seamLedgerRows(t, pool, "ws"), "spend")
	if spend.meta["price_basis"] != "fallback" {
		t.Errorf("spend row price_basis = %v, want \"fallback\" — a guessed rate must say it is a guess (meta: %v)",
			spend.meta["price_basis"], spend.meta)
	}
}

// A KNOWN model must be completely unaffected: exact pricing, and no fallback marking.
func TestKnownModel_UnchangedAndNotMarkedFallback(t *testing.T) {
	p, store, pool := seamProxy(t)
	ctx := context.Background()
	seamFund(t, pool, "ws", 100_000_000)
	rctx, _ := p.agentReserveBlocks(ctx, "agent", "ws", "gpt-4o", "prompt", "rq-known", 4096)
	_ = store
	served, prov := alerts.CostUSDResolved("gpt-4o", catalog.PurposeCharge, 2_000, 0, 0, 1_000)
	if prov != catalog.ProvenanceExact {
		t.Fatalf("gpt-4o provenance = %v, want exact", prov)
	}
	basis := ""
	if prov == catalog.ProvenanceFallback {
		basis = prov.String()
	}
	p.settleReservationBasis(rctx, served, "gpt-4o", basis)

	spend := findRowByType(t, seamLedgerRows(t, pool, "ws"), "spend")
	if _, marked := spend.meta["price_basis"]; marked {
		t.Errorf("a KNOWN model's spend row carries price_basis=%v — exact pricing must not be marked as a guess",
			spend.meta["price_basis"])
	}
	if spend.amount != -usdToULXC(served) {
		t.Errorf("known-model charge = %d, want %d (unchanged by this fix)", spend.amount, -usdToULXC(served))
	}
}
