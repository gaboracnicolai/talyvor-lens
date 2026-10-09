package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

type recordedAlert struct{ kind, subject, body string }

type alertRecorder struct{ got []recordedAlert }

func (a *alertRecorder) NotifyAs(_ context.Context, kind, _, subject, body string) error {
	a.got = append(a.got, recordedAlert{kind, subject, body})
	return nil
}

// B30.11 — a day whose ledger has a payment the partner's statement lacks alerts the operator once, and the break and
// the shortfall it leaves are at /v1/admin/reconciliation and /v1/admin/safeguarding.
func TestReconciliation_ABreakAlertsTheOperatorAndShowsOnTheRoutes(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	registry := partners.NewRegistry(nil)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetScreener(screening.NewScreener(pool, registry))
	const ws = "ws-b3011-routes"

	p, err := registry.Account(ctx, economy.CapabilityCurrencyAccounts)
	if err != nil {
		t.Fatal(err)
	}
	at, err := p.OpenAccount(ctx, partners.AccountRequest{ID: "acct-eur", Holder: "Acme Ltd", Currency: economy.CurrencyEUR})
	if err != nil {
		t.Fatal(err)
	}
	company, err := store.OpenMoneyAccount(ctx, economy.MoneyAccount{WorkspaceID: ws, Currency: economy.CurrencyEUR, Purpose: economy.MoneyCompany})
	if err != nil {
		t.Fatal(err)
	}
	partner, err := store.OpenMoneyAccount(ctx, economy.MoneyAccount{WorkspaceID: ws, Currency: economy.CurrencyEUR, Purpose: economy.MoneyPartner,
		PartnerAccountRef: at.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityCurrencyAccounts, Counterparty: "Acme Ltd",
		Kind: "payment_in", IdempotencyKey: "in-1", PartnerRef: "test_pbb_never_arrived", Funding: economy.FundingTest,
		Postings: []economy.MoneyPosting{{AccountID: company.ID, AmountMinor: 40_00}, {AccountID: partner.ID, AmountMinor: -40_00}}}); err != nil {
		t.Fatal(err)
	}

	sink := &alertRecorder{}
	if err := reconcileOnce(ctx, store, registry, sink, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(sink.got) != 1 || sink.got[0].kind != "reconciliation_break" || !strings.Contains(sink.got[0].body, "EUR: 1 breaks") {
		t.Fatalf("the operator was sent %+v; want one reconciliation_break alert naming EUR's 1 break", sink.got)
	}

	var runs struct{ Runs []economy.ReconciliationRun }
	reconciliationGET(t, newReconciliationRunsHandler(store), &runs)
	var eur economy.ReconciliationRun
	for _, r := range runs.Runs {
		if r.Currency == economy.CurrencyEUR {
			eur = r
		}
	}
	if eur.BreakCount != 1 || eur.Breaks[0].Kind != economy.BreakMissing || eur.Breaks[0].LedgerMinor != 40_00 {
		t.Fatalf("GET /v1/admin/reconciliation shows EUR as %+v; want its €40.00 missing break", eur)
	}
	var view struct{ Currencies []economy.ReconciliationRun }
	reconciliationGET(t, newSafeguardingHandler(store), &view)
	for _, c := range view.Currencies {
		if c.Currency == economy.CurrencyEUR && (c.CustomersHoldMinor != 40_00 || c.PartnerHoldsMinor != 0 || c.ShortfallMinor != 40_00) {
			t.Fatalf("GET /v1/admin/safeguarding shows EUR as %+v; want €40.00 held by customers, none by the partner", c)
		}
	}
	if len(view.Currencies) != 4 {
		t.Fatalf("GET /v1/admin/safeguarding shows %d currencies; want GBP, EUR, USD and USDC", len(view.Currencies))
	}
}

func reconciliationGET(t *testing.T, h http.Handler, into any) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET answered %d: %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
		t.Fatal(err)
	}
}
