package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/tenant"
)

// B17.15 — on a Lens with no marketplace bill in Stripe for anyone (production's, today), a test company's
// agent still pays another test company's agent: one unpaid line on the payer's bill, kept by Lens; the
// payee's share is pending until that bill is paid (B25.7's synthetic bill-pay), then earned past the
// holdback. A real company there has no bill, so its payment is refused and nothing is recorded.
func TestB1715_ATestCompanysAgentPaysAnotherCompanysAgentOnALensKeptBill(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const tPayer, tPayee, rPayer, rPayee = "s-b1715-payer", "s-b1715-payee", "u-b1715-payer", "u-b1715-payee"
	for _, ws := range []string{tPayer, tPayee, rPayer, rPayee} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, $2)`,
			ws, strings.HasPrefix(ws, "s-")); err != nil {
			t.Fatal(err)
		}
	}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	kinds := newStripeByKind(func(ws string) bool { return strings.HasPrefix(ws, "s-") }, false, false, stripeSide{}, stripeSide{})
	bank.SetCompanyPayments(companyPaymentsOnBill{Store: store, bills: kinds})
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, bank, tenant.NewStore(pool))
	mountMarketUseRoutes(r, store, http.NotFoundHandler(), kinds, bank)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	agent := func(ws, name string) string {
		t.Helper()
		a, err := bank.CreateAgent(ctx, ws, name, "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	pay := func(ws, from, to string) (int, string) {
		t.Helper()
		return call(ws, http.MethodPost, "/v1/workspaces/"+ws+"/agents/"+from+"/pay",
			fmt.Sprintf(`{"to_agent_id":%q,"amount_ulxc":700000,"memo":"nightly check"}`, to))
	}
	buyer, supplier := agent(tPayer, "Buyer"), agent(tPayee, "Supplier")

	code, body := pay(tPayer, buyer, supplier)
	var paid economy.AgentPayment
	if _ = json.Unmarshal([]byte(body), &paid); code != http.StatusOK || paid.Via != "marketplace" || paid.ToWorkspaceID != tPayee {
		t.Fatalf("the test company's payment = %d %s, want it through the marketplace to %s", code, body, tPayee)
	}
	code, body = call(tPayer, http.MethodGet, "/v1/workspaces/"+tPayer+"/marketplace/bill", "")
	var bill market.Bill
	if _ = json.Unmarshal([]byte(body), &bill); code != http.StatusOK || len(bill.Lines) != 1 || bill.Lines[0].UseID != paid.EntryID ||
		bill.Lines[0].Title != "Payment to Supplier" || bill.Lines[0].PriceULXC != 700_000 || bill.Lines[0].Cleared != nil {
		t.Fatalf("the payer's bill = %d %s, want one unpaid 0.7 LXC line \"Payment to Supplier\"", code, body)
	}
	now := time.Now()
	if e, err := store.SellerEarnings(ctx, tPayee, now); err != nil || e.PendingUses != 1 || e.PendingUSDMicros != 70_000 ||
		e.PayableUSDMicros != 0 || e.AvailableUSDMicros != 0 {
		t.Fatalf("before the bill is paid the payee's earnings = %+v (%v), want $0.07 pending and nothing payable", e, err)
	}

	// The metering pass puts it on the Lens-kept bill; the synthetic bill-pay pays it, past the holdback.
	if n, err := store.MeterPending(ctx, kinds, 0); err != nil || n != 1 {
		t.Fatalf("metering = %d (%v), want the payment on the bill once", n, err)
	}
	if _, n, err := store.PayTestBill(ctx, tPayer, now); err != nil || n != 1 {
		t.Fatalf("paying the test bill cleared %d use(s) (%v), want 1", n, err)
	}
	if e, err := store.SellerEarnings(ctx, tPayee, now); err != nil || e.PendingUses != 0 || e.AvailableUSDMicros != 70_000 {
		t.Fatalf("after the bill is paid the payee's earnings = %+v (%v), want $0.07 available", e, err)
	}

	// A real company on this Lens has no bill: refused, and nothing is recorded.
	realBuyer := agent(rPayer, "Real buyer")
	if code, body := pay(rPayer, realBuyer, agent(rPayee, "Real supplier")); code != http.StatusForbidden || !strings.Contains(body, "marketplace bill") {
		t.Fatalf("a real company's payment with no bill = %d %s, want 403 naming the marketplace bill", code, body)
	}
	var recorded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE agent_id = $1`, realBuyer).Scan(&recorded); err != nil || recorded != 0 {
		t.Fatalf("the refused payment left %d market_uses row(s) (%v), want none", recorded, err)
	}
}
