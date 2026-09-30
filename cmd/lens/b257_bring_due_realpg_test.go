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

	"github.com/talyvor/lens/internal/agentcard"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/workspace"
)

// B25.7 — A TEST USER'S SLOW MONEY CAN BE BROUGHT DUE INSIDE ONE TESTER RUN.
//
// Two test users made as production makes them, wired as main.go wires them, on the migrated schema. Each of
// the four synthetic-key routes brings its slow money due now, and the rows B25.3's script checks follow, every
// one marked test: a loan's instalment taken by the minute tick, and another's missed and then defaulted; a
// bill paid with its earnings past the holdback, then taken as credits; a paid bill refunded; a purchase on an
// agent's card. Called for a real workspace, each is refused and no row changes.
func TestB257_ATestUsersSlowMoneyIsBroughtDueInsideOneRun(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const syntheticKey = "b257-synthetic-key"

	ecb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
  <Cube><Cube time="`+time.Now().UTC().Format("2006-01-02")+`"><Cube currency="USD" rate="1.1700"/><Cube currency="GBP" rate="0.8700"/></Cube></Cube>
</gesmes:Envelope>`)
	}))
	defer ecb.Close()

	// Wired as main.go wires it.
	marketStore := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetListingCharger(marketStore)
	bank.SetCompanyPayments(marketStore)
	bank.SetOwnerVerifier(earnverify.New(false))
	billFake := &marketStripe{}
	svc := billing.New(pool, bank, billFake, "whsec_b257").WithMarketBill(billFake, "price_market", "talyvor_marketplace_use", marketStore)
	lens := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	})
	r := chi.NewRouter()
	mountSyntheticRoutes(r, syntheticKey, syntheticDeps{workspaces: workspace.New(pool), credits: bank, answers: storedanswers.New(pool, nil),
		audit: pool, mint: func(ws, _ string, _ []string, _ time.Duration) (string, error) { return "tok-" + ws, nil },
		due: syntheticDueDeps{db: pool, loans: bank, bills: marketStore, cards: bank,
			authorizer: agentcard.NewHandler("", bank, ecbrate.New(pool, ecb.URL))}})
	mountAgentAccountRoutes(r, bank, tenant.NewStore(pool))
	mountCompanyLoanRoutes(r, bank)
	mountAgentCardRoutes(r, bank, &fakeCardIssuer{})
	mountMarketRoutes(r, marketStore)
	mountMarketUseRoutes(r, marketStore, lens, svc, bank)
	mountMarketPayoutRoutes(r, marketStore, everyWorkspace(&connectFake{}), bank, marketPayoutURLs{})

	synthetic := func(path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set(syntheticKeyHeader, syntheticKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	do := func(what string, want int, ws, method, path, body string, out any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: ws, Scopes: syntheticScopes}))
		w := httptest.NewRecorder()
		if r.ServeHTTP(w, req); w.Code != want {
			t.Fatalf("%s: %s %s = %d %s, want %d", what, method, path, w.Code, w.Body.String(), want)
		}
		if out != nil {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s: %v in %s", what, err, w.Body.String())
			}
		}
	}
	due := func(what, path, body string, out any) {
		t.Helper()
		code, got := synthetic(path, body)
		if code != http.StatusOK {
			t.Fatalf("%s: POST %s = %d %s, want 200", what, path, code, got)
		}
		if out != nil {
			if err := json.Unmarshal([]byte(got), out); err != nil {
				t.Fatalf("%s: %v in %s", what, err, got)
			}
		}
	}
	// testRows asserts want rows of table where cond, every one marked test.
	testRows := func(what, table, cond string, want int, args ...any) {
		t.Helper()
		var n, test int
		if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE test) FROM `+table+` WHERE `+cond, args...).Scan(&n, &test); err != nil {
			t.Fatalf("%s: %s: %v", what, table, err)
		}
		if n != want || test != n {
			t.Errorf("%s: %d %s rows where %s (%d marked test), want %d, every one test", what, n, table, cond, test, want)
		}
	}
	tick := func() economy.LoanRunResult { // the minute tick, as main.go runs it
		t.Helper()
		res, err := bank.RunLoanRepayments(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// The two test users, and their agents funded from their test credits.
	code, got := synthetic("/v1/synthetic/workspaces", `{"count":2}`)
	var made struct {
		Workspaces []syntheticUser `json:"workspaces"`
	}
	if _ = json.Unmarshal([]byte(got), &made); code != http.StatusCreated || len(made.Workspaces) != 2 {
		t.Fatalf("make two test users = %d %s", code, got)
	}
	A, B := made.Workspaces[0].WorkspaceID, made.Workspaces[1].WorkspaceID
	var a, b economy.Agent
	do("A makes an agent", http.StatusCreated, A, http.MethodPost, "/v1/workspaces/"+A+"/agents", `{"name":"a-agent"}`, &a)
	do("B makes an agent", http.StatusCreated, B, http.MethodPost, "/v1/workspaces/"+B+"/agents", `{"name":"b-agent"}`, &b)
	aBase, bBase := "/v1/workspaces/"+A+"/agents/"+a.ID, "/v1/workspaces/"+B+"/agents/"+b.ID
	do("A funds its agent", http.StatusOK, A, http.MethodPost, aBase+"/fund", `{"amount_ulxc":400000000}`, nil)
	do("B funds its agent", http.StatusOK, B, http.MethodPost, bBase+"/fund", `{"amount_ulxc":400000000}`, nil)

	// A loan's instalment, due a week after acceptance, brought due now: the minute tick takes it.
	var loan economy.Loan
	do("loan: offer", http.StatusCreated, A, http.MethodPost, aBase+"/loans",
		fmt.Sprintf(`{"to":%q,"principal_ulxc":50000000,"interest_bps":1000,"instalments":1,"every":"week"}`, b.ID), &loan)
	do("loan: accept", http.StatusOK, B, http.MethodPost, "/v1/workspaces/"+B+"/loans/"+loan.ID+"/accept", "", nil)
	if res := tick(); res.Paid != 0 {
		t.Fatalf("loan: the tick took %+v before the instalment was due", res)
	}
	due("loan: due", "/v1/synthetic/workspaces/"+B+"/loans/"+loan.ID+"/due", "", &loan)
	if loan.NextDueAt == nil || loan.NextDueAt.After(time.Now()) {
		t.Fatalf("loan: due now, the loan's next instalment is due at %v", loan.NextDueAt)
	}
	if res := tick(); res.Paid != 1 {
		t.Fatalf("loan: the tick after it was brought due = %+v, want the instalment taken", res)
	}
	testRows("loan: repaid", "agent_loans", "id = $1 AND status = 'repaid'", 1, loan.ID)
	testRows("loan: repaid", "agent_transfers", "loan_id = $1 AND from_workspace_id = $2 AND to_workspace_id = $3 AND amount_ulxc = 55000000", 1, loan.ID, B, A)

	// Another, whose borrower cannot pay: brought due, missed and late; brought due again, in default.
	var bad economy.Loan
	do("default: offer", http.StatusCreated, A, http.MethodPost, aBase+"/loans",
		fmt.Sprintf(`{"to":%q,"principal_ulxc":20000000,"instalments":2,"every":"day","late_fee_ulxc":1000000}`, b.ID), &bad)
	do("default: accept", http.StatusOK, B, http.MethodPost, "/v1/workspaces/"+B+"/loans/"+bad.ID+"/accept", "", nil)
	var book economy.AgentBook
	do("default: B's agents", http.StatusOK, B, http.MethodGet, "/v1/workspaces/"+B+"/agents", "", &book)
	for _, x := range book.Agents {
		do("default: B's agent spends everything", http.StatusOK, B, http.MethodPost, bBase+"/withdraw", fmt.Sprintf(`{"amount_ulxc":%d}`, x.BalanceULXC), nil)
	}
	// (The tick's counts are not asserted: it reports a missed instalment as nothing done — in FOUND.md.)
	due("default: due", "/v1/synthetic/workspaces/"+A+"/loans/"+bad.ID+"/due", "", nil)
	tick()
	testRows("default: missed", "agent_loans", "id = $1 AND status = 'late'", 1, bad.ID)
	due("default: due again", "/v1/synthetic/workspaces/"+A+"/loans/"+bad.ID+"/due", "", nil)
	tick()
	testRows("default", "agent_loans", "id = $1 AND status = 'defaulted'", 1, bad.ID)
	testRows("default", "agent_transfers", "loan_id = $1 AND from_workspace_id = $2 AND to_workspace_id = $3", 1, bad.ID, A, B)
	var events string
	if err := pool.QueryRow(ctx, `SELECT string_agg(kind, ',' ORDER BY id) FROM agent_loan_events WHERE loan_id = $1`, bad.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != "payout,missed,late,missed,defaulted" {
		t.Errorf("default: the loan's events are %s, want payout,missed,late,missed,defaulted", events)
	}
	do("B funds its agent again", http.StatusOK, B, http.MethodPost, bBase+"/fund", `{"amount_ulxc":400000000}`, nil)

	// B uses A's listing on its bill; the bill paid now, its earning is past the holdback and A takes it.
	var listing market.Listing
	do("list", http.StatusCreated, A, http.MethodPost, "/v1/workspaces/"+A+"/marketplace/listings",
		`{"kind":"prompt","title":"Contract review","price_per_use_ulxc":100000000,"artifact":{"template":"Review {{text}}","model":"m"}}`, &listing)
	use := func() string {
		t.Helper()
		var u market.Use
		do("use", http.StatusOK, B, http.MethodPost, "/v1/workspaces/"+B+"/marketplace/listings/"+listing.ID+"/use", `{"variables":{"text":"the NDA"}}`, &u)
		if u.Charge != market.ChargeBilled {
			t.Fatalf("use: the use was %s, want billed", u.Charge)
		}
		return u.ID
	}
	paid := use()
	var bill struct {
		InvoiceID   string `json:"invoice_id"`
		UsesCleared int    `json:"uses_cleared"`
	}
	due("bill: pay", "/v1/synthetic/workspaces/"+B+"/marketplace/bill/pay", "", &bill)
	if bill.UsesCleared != 1 || bill.InvoiceID == "" {
		t.Fatalf("bill: paid %+v, want the one use cleared", bill)
	}
	testRows("bill: pay", "market_uses", "id = $1 AND cleared_invoice_id = $2", 1, paid, bill.InvoiceID)
	testRows("bill: pay", "market_earnings", "use_id = $1 AND seller_workspace_id = $2 AND payable_at <= now()", 1, paid, A)
	do("payout: take as credits", http.StatusCreated, A, http.MethodPost, "/v1/workspaces/"+A+"/marketplace/payouts/credits", "", nil)
	testRows("payout", "market_payouts", "workspace_id = $1 AND method = 'credits' AND gross_usd_micros > 0", 1, A)

	// Another use on another bill, paid and then refunded as charge.refunded refunds it.
	refunded := use()
	due("refund: the bill paid", "/v1/synthetic/workspaces/"+B+"/marketplace/bill/pay", "", &bill)
	due("refund", "/v1/synthetic/workspaces/"+B+"/marketplace/bill/"+bill.InvoiceID+"/refund", "", nil)
	testRows("refund", "market_refunds", "use_id = $1 AND cause = 'buyer_refund' AND buyer_workspace_id = $2 AND seller_workspace_id = $3", 1, refunded, B, A)

	// A purchase on A's agent's card, decided as Stripe Issuing's authorisation request is.
	do("card: issue", http.StatusCreated, A, http.MethodPost, aBase+"/card",
		`{"first_name":"Test","last_name":"User","line1":"1 High Street","city":"London","postal_code":"EC1A 1BB"}`, nil)
	var auth struct {
		AuthorizationID string `json:"authorization_id"`
		Approved        bool   `json:"approved"`
		Reason          string `json:"reason"`
	}
	due("card: purchase", "/v1/synthetic/workspaces/"+A+"/agents/"+a.ID+"/card/authorizations", `{"amount_minor":2000,"currency":"gbp","merchant":"Paper Co"}`, &auth)
	if !auth.Approved {
		t.Fatalf("card: a £20 purchase was declined: %s", auth.Reason)
	}
	testRows("card", "agent_card_authorizations", "workspace_id = $1 AND agent_id = $2 AND authorization_id = $3 AND approved", 1, A, a.ID, auth.AuthorizationID)
	testRows("card", "agent_postings", "workspace_id = $1 AND ref = $2", 2, A, auth.AuthorizationID)

	// A real workspace: each route is refused, and no row changes.
	const real = "ws-real-b257"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, real); err != nil {
		t.Fatal(err)
	}
	rows := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT concat_ws('|',
			(SELECT string_agg(id || ':' || status || ':' || COALESCE(next_due_at::text, ''), ',' ORDER BY id) FROM agent_loans),
			(SELECT count(*) FROM agent_loan_events), (SELECT count(*) FROM agent_transfers), (SELECT count(*) FROM market_uses WHERE cleared_at IS NOT NULL),
			(SELECT count(*) FROM market_earnings), (SELECT count(*) FROM market_refunds), (SELECT count(*) FROM market_payouts),
			(SELECT count(*) FROM agent_card_authorizations), (SELECT count(*) FROM agent_postings))`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := rows()
	for _, path := range []string{
		"/v1/synthetic/workspaces/" + real + "/loans/" + loan.ID + "/due",
		"/v1/synthetic/workspaces/" + real + "/marketplace/bill/pay",
		"/v1/synthetic/workspaces/" + real + "/marketplace/bill/" + bill.InvoiceID + "/refund",
		"/v1/synthetic/workspaces/" + real + "/agents/" + a.ID + "/card/authorizations",
	} {
		if code, got := synthetic(path, ""); code != http.StatusForbidden {
			t.Errorf("a real workspace: POST %s = %d %s, want 403", path, code, got)
		}
	}
	if after := rows(); after != before {
		t.Errorf("a real workspace's calls changed rows:\n before %s\n after  %s", before, after)
	}
}
