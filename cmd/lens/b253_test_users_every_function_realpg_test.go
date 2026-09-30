package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/agentcard"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/moderatorkey"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/workspace"
)

// b253MoneyTables is every table migration 0173 marks, and the workspace columns its mark follows.
var b253MoneyTables = map[string][]string{
	"lxc_ledger": {"workspace_id"}, "lxc_purchases": {"workspace_id"}, "lens_token_ledger": {"workspace_id"},
	"pool_royalty_mints":    {"requester_workspace_id", "contributor_workspace_id"},
	"distill_royalty_mints": {"requester_workspace_id", "contributor_workspace_id"},
	"agent_postings":        {"workspace_id"}, "agent_transfers": {"from_workspace_id", "to_workspace_id"},
	"agent_money_requests": {"from_workspace_id", "to_workspace_id"}, "agent_loans": {"lender_workspace_id", "borrower_workspace_id"},
	"agent_escrows": {"payer_workspace_id", "payee_workspace_id"}, "agent_pots": {"workspace_id"},
	"agent_payment_schedules": {"workspace_id"}, "agent_topups": {"workspace_id"}, "agent_cards": {"workspace_id"},
	"agent_card_authorizations": {"workspace_id"}, "agent_card_settlements": {"workspace_id"}, "agent_cash_outs": {"workspace_id"},
	"agent_debit_settlements": {"workspace_id"}, "market_uses": {"buyer_workspace_id", "seller_workspace_id"},
	"market_earnings": {"seller_workspace_id"}, "market_refunds": {"buyer_workspace_id", "seller_workspace_id"},
	"market_payouts": {"workspace_id"},
}

// B25.3 — EVERY WALLET, BANK AND MARKETPLACE FUNCTION WORKS FOR TEST USERS, AMONG TEST USERS.
//
// The script: two test users are made as production makes them — POST /v1/synthetic/workspaces, a synthetic
// workspace with its 1,000 granted test credits and no purchase — and every function is driven between them
// through the real routes, wired as main.go wires them, on the migrated schema. Each ends with its rows on
// both sides, every money row marked test by the database (migration 0173). Stripe (the marketplace bill,
// Connect, Issuing) is a test-mode fake; the ticks (instalments, schedules, cash-outs, payouts) are called
// as the leader's tick calls them. It logs every function and its state.
func TestB253_EveryWalletBankAndMarketplaceFunctionBetweenTwoTestUsers(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const secret, syntheticKey = "whsec_b253", "b253-synthetic-key"
	now := time.Now()

	// Wired as main.go wires it.
	marketStore := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetListingCharger(marketStore)
	bank.SetCompanyPayments(marketStore)
	bank.SetOwnerVerifier(earnverify.New(false))
	bank.SetCashOutPartner(economy.TestCashOutPartner{})
	billFake := &marketStripe{}
	connect := &connectFake{accounts: map[string]*billing.ConnectAccount{}, charges: map[string]connectCharge{}}
	svc := billing.New(pool, bank, billFake, secret).
		WithMarketBill(billFake, "price_market", "talyvor_marketplace_use", marketStore).
		WithMarketPayouts(connect, marketStore)
	lens := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	})
	moderators := moderatorkey.NewStore(pool)
	am := auth.NewManager("b253-admin-key", nil, auth.New(pool), nil).WithModeratorKeys(moderators)
	r := chi.NewRouter()
	mountSyntheticRoutes(r, syntheticKey, syntheticDeps{workspaces: workspace.New(pool), credits: bank, answers: storedanswers.New(pool, nil),
		audit: pool, mint: func(ws, _ string, _ []string, _ time.Duration) (string, error) { return "tok-" + ws, nil }})
	mountAgentAccountRoutes(r, bank, tenant.NewStore(pool))
	mountAgentTransferRoutes(r, bank)
	mountCompanyLoanRoutes(r, bank)
	mountAgentEscrowRoutes(r, bank)
	mountAgentPotRoutes(r, bank)
	mountAgentCardRoutes(r, bank, &fakeCardIssuer{})
	mountCashOutRoutes(r, bank)
	mountMarketRoutes(r, marketStore)
	mountMarketUseRoutes(r, marketStore, lens, svc, bank)
	mountMarketPayoutRoutes(r, marketStore, everyWorkspace(connect), bank, marketPayoutURLs{refresh: "https://app.test/expired", ret: "https://app.test/selling"})
	r.Get("/v1/admin/marketplace/review", requireAdminOrModerator(am, moderators, newMarketReviewQueueHandler(marketStore)))
	r.Post("/v1/admin/marketplace/listings/{listingID}/approve", requireAdminOrModerator(am, moderators, newMarketApproveHandler(marketStore)))
	r.Post("/v1/admin/marketplace/listings/{listingID}/takedown", requireAdminOrModerator(am, moderators, newMarketTakedownHandler(marketStore, svc)))
	r.Post("/v1/billing/webhook", svc.HandleWebhook)

	// The two test users.
	req := httptest.NewRequest(http.MethodPost, "/v1/synthetic/workspaces", strings.NewReader(`{"count":2}`))
	req.Header.Set(syntheticKeyHeader, syntheticKey)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var made struct {
		Workspaces []syntheticUser `json:"workspaces"`
	}
	if _ = json.Unmarshal(rec.Body.Bytes(), &made); rec.Code != http.StatusCreated || len(made.Workspaces) != 2 {
		t.Fatalf("make two test users = %d %s", rec.Code, rec.Body.String())
	}
	A, B := made.Workspaces[0].WorkspaceID, made.Workspaces[1].WorkspaceID

	// Each call is the test user's own token: its workspace, its person, the synthetic scopes.
	call := func(ws, method, path, body string, header ...string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok-"+ws)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: ws, Scopes: syntheticScopes}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	do := func(what string, want int, ws, method, path, body string, out any) {
		t.Helper()
		code, got := call(ws, method, path, body)
		if code != want {
			t.Fatalf("%s: %s %s = %d %s, want %d", what, method, path, code, got, want)
		}
		if out != nil {
			if err := json.Unmarshal([]byte(got), out); err != nil {
				t.Fatalf("%s: %v in %s", what, err, got)
			}
		}
	}
	// What each function left, for the log: function → the rows that show it.
	var functions []string
	state := map[string][]string{}
	note := func(fn, format string, args ...any) {
		if _, ok := state[fn]; !ok {
			functions = append(functions, fn)
		}
		state[fn] = append(state[fn], fmt.Sprintf(format, args...))
	}
	// testRows asserts want rows of table where cond, every one marked test, and notes them under fn.
	testRows := func(fn, table, cond string, want int, args ...any) {
		t.Helper()
		var n, test int
		if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE test) FROM `+table+` WHERE `+cond, args...).Scan(&n, &test); err != nil {
			t.Fatalf("%s: %s: %v", fn, table, err)
		}
		if n != want || test != n {
			t.Errorf("%s: %d %s rows where %s (%d marked test), want %d, every one test", fn, n, table, cond, test, want)
		}
		note(fn, "%d %s (test)", n, table)
	}
	// value reads one value, and notes it under fn.
	value := func(fn, what, query string, args ...any) string {
		t.Helper()
		var v string
		if err := pool.QueryRow(ctx, query, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %s: %v", fn, what, err)
		}
		note(fn, "%s %s", what, v)
		return v
	}
	postings := func(fn, ws, ref string, want int) {
		t.Helper()
		testRows(fn, "agent_postings", "workspace_id = $1 AND ref = $2", want, ws, ref)
	}
	agentsOf := func(ws string) economy.AgentBook {
		t.Helper()
		var book economy.AgentBook
		do("the agent book", http.StatusOK, ws, http.MethodGet, "/v1/workspaces/"+ws+"/agents", "", &book)
		return book
	}

	// Each test user makes an agent and funds it from its test credits — nothing bought.
	var a, b economy.Agent
	do("A makes an agent", http.StatusCreated, A, http.MethodPost, "/v1/workspaces/"+A+"/agents", `{"name":"a-agent"}`, &a)
	do("B makes an agent", http.StatusCreated, B, http.MethodPost, "/v1/workspaces/"+B+"/agents", `{"name":"b-agent"}`, &b)
	aBase, bBase := "/v1/workspaces/"+A+"/agents/"+a.ID, "/v1/workspaces/"+B+"/agents/"+b.ID
	do("A funds its agent", http.StatusOK, A, http.MethodPost, aBase+"/fund", `{"amount_ulxc":400000000}`, nil)
	do("B funds its agent", http.StatusOK, B, http.MethodPost, bBase+"/fund", `{"amount_ulxc":400000000}`, nil)

	// ── Agent Wallets ──

	var sent economy.AgentTransfer
	do("send", http.StatusOK, A, http.MethodPost, aBase+"/send", fmt.Sprintf(`{"to":%q,"amount_ulxc":10000000,"memo":"lunch"}`, b.ID), &sent)
	testRows("send", "agent_transfers", "id = $1 AND from_workspace_id = $2 AND to_workspace_id = $3", 1, sent.ID, A, B)
	postings("send", A, sent.ID, 1)
	postings("send", B, sent.ID, 1)

	var asked economy.MoneyRequest
	do("request", http.StatusCreated, B, http.MethodPost, bBase+"/requests", fmt.Sprintf(`{"from":%q,"amount_ulxc":5000000,"memo":"invoice 7"}`, a.ID), &asked)
	do("request: the payer pays it", http.StatusOK, A, http.MethodPost, "/v1/workspaces/"+A+"/money-requests/"+asked.ID+"/accept", "", &asked)
	testRows("request", "agent_money_requests", "id = $1 AND status = 'accepted'", 1, asked.ID)
	testRows("request", "agent_transfers", "id = $1 AND from_workspace_id = $2 AND to_workspace_id = $3", 1, asked.TransferID, A, B)
	postings("request", A, asked.TransferID, 1)
	postings("request", B, asked.TransferID, 1)

	var back economy.AgentTransfer
	do("refund a transfer", http.StatusOK, B, http.MethodPost, "/v1/workspaces/"+B+"/transfers/"+sent.ID+"/refund", "", &back)
	testRows("refund a transfer", "agent_transfers", "refund_of = $1 AND from_workspace_id = $2 AND to_workspace_id = $3", 1, sent.ID, B, A)
	postings("refund a transfer", A, back.ID, 1)
	postings("refund a transfer", B, back.ID, 1)

	// Loans: A lends to B; one is repaid, and one defaults.
	var loan economy.Loan
	do("loan: offer", http.StatusCreated, A, http.MethodPost, aBase+"/loans",
		fmt.Sprintf(`{"to":%q,"principal_ulxc":50000000,"interest_bps":1000,"instalments":1,"every":"week","memo":"bridge"}`, b.ID), &loan)
	testRows("loan: offer", "agent_loans", "id = $1 AND lender_workspace_id = $2 AND borrower_workspace_id = $3 AND status = 'offered'", 1, loan.ID, A, B)
	do("loan: accept", http.StatusOK, B, http.MethodPost, "/v1/workspaces/"+B+"/loans/"+loan.ID+"/accept", "", &loan)
	testRows("loan: accept", "agent_transfers", "loan_id = $1 AND from_workspace_id = $2 AND to_workspace_id = $3", 1, loan.ID, A, B)
	if res, err := bank.RunLoanRepayments(ctx, now.AddDate(0, 0, 8)); err != nil || res.Paid != 1 {
		t.Fatalf("loan: repay: the instalment run = %+v, %v; want one paid", res, err)
	}
	testRows("loan: repay", "agent_transfers", "loan_id = $1 AND from_workspace_id = $2 AND to_workspace_id = $3 AND amount_ulxc = 55000000", 1, loan.ID, B, A)
	testRows("loan: repay", "agent_loans", "id = $1 AND status = 'repaid'", 1, loan.ID)
	var bad economy.Loan
	do("loan: default", http.StatusCreated, A, http.MethodPost, aBase+"/loans",
		fmt.Sprintf(`{"to":%q,"principal_ulxc":20000000,"instalments":2,"every":"day","late_fee_ulxc":1000000,"memo":"risky"}`, b.ID), &bad)
	do("loan: default", http.StatusOK, B, http.MethodPost, "/v1/workspaces/"+B+"/loans/"+bad.ID+"/accept", "", nil)
	for _, x := range agentsOf(B).Agents { // B's agent spends everything, so it cannot pay
		do("loan: default", http.StatusOK, B, http.MethodPost, bBase+"/withdraw", fmt.Sprintf(`{"amount_ulxc":%d}`, x.BalanceULXC), nil)
	}
	for _, day := range []int{2, 3} {
		if _, err := bank.RunLoanRepayments(ctx, now.AddDate(0, 0, day)); err != nil {
			t.Fatal(err)
		}
	}
	testRows("loan: default", "agent_loans", "id = $1 AND status = 'defaulted'", 1, bad.ID)
	value("loan: default", "events", `SELECT string_agg(kind, ',' ORDER BY id) FROM agent_loan_events WHERE loan_id = $1`, bad.ID)
	do("B funds its agent again", http.StatusOK, B, http.MethodPost, bBase+"/fund", `{"amount_ulxc":400000000}`, nil)

	// Escrow: one released on delivery, one disputed and decided by the operator.
	escrow := func(fn, memo string) economy.Escrow {
		t.Helper()
		var e economy.Escrow
		do(fn, http.StatusCreated, A, http.MethodPost, aBase+"/escrows",
			fmt.Sprintf(`{"to":%q,"amount_ulxc":8000000,"release_at":%q,"memo":%q}`, b.ID, now.Add(7*24*time.Hour).Format(time.RFC3339), memo), &e)
		return e
	}
	held := escrow("escrow: open", "logo design")
	testRows("escrow: open", "agent_escrows", "id = $1 AND payer_workspace_id = $2 AND payee_workspace_id = $3 AND status = 'held'", 1, held.ID, A, B)
	postings("escrow: open", A, held.ID, 2)
	do("escrow: release", http.StatusOK, A, http.MethodPost, "/v1/workspaces/"+A+"/escrows/"+held.ID+"/confirm", "", nil)
	testRows("escrow: release", "agent_escrows", "id = $1 AND status = 'released'", 1, held.ID)
	postings("escrow: release", B, held.ID, 1)
	disputed := escrow("escrow: dispute", "never delivered")
	do("escrow: dispute", http.StatusOK, A, http.MethodPost, "/v1/workspaces/"+A+"/escrows/"+disputed.ID+"/dispute", `{"reason":"nothing arrived"}`, nil)
	testRows("escrow: dispute", "agent_escrows", "id = $1 AND status = 'disputed'", 1, disputed.ID)
	if _, err := bank.DecideEscrow(ctx, disputed.ID, false, "operator", "refund the payer"); err != nil {
		t.Fatalf("escrow: dispute: the operator's decision: %v", err)
	}
	value("escrow: dispute", "decided", `SELECT status FROM agent_escrows WHERE id = $1`, disputed.ID)
	postings("escrow: dispute", A, disputed.ID, 4) // paid in, then returned

	// Pots.
	var pot economy.Pot
	do("pots", http.StatusCreated, A, http.MethodPost, aBase+"/pots", `{"name":"holiday","kind":"goal","target_ulxc":30000000}`, &pot)
	do("pots", http.StatusOK, A, http.MethodPost, aBase+"/pots/"+pot.ID+"/in", `{"amount_ulxc":12000000}`, nil)
	do("pots", http.StatusOK, A, http.MethodPost, aBase+"/pots/"+pot.ID+"/out", `{"amount_ulxc":2000000}`, &pot)
	testRows("pots", "agent_pots", "id = $1 AND workspace_id = $2", 1, pot.ID, A)
	testRows("pots", "agent_postings", "workspace_id = $1 AND account = $2", 2, A, "pot:"+pot.ID)
	if pot.BalanceULXC != 10_000_000 {
		t.Errorf("pots: the pot holds %d µLXC, want 10 LXC", pot.BalanceULXC)
	}

	// Recurring transfers: A pays B's agent every day.
	var schedule economy.AgentSchedule
	do("recurring transfers", http.StatusCreated, A, http.MethodPost, aBase+"/schedules",
		fmt.Sprintf(`{"to_agent_id":%q,"amount_ulxc":3000000,"memo":"retainer","every":"day","first_run_at":%q}`, b.ID, now.Add(-time.Minute).Format(time.RFC3339)), &schedule)
	for _, at := range []time.Time{now, now.Add(24 * time.Hour)} {
		if _, err := bank.RunAgentSchedules(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	testRows("recurring transfers", "agent_payment_schedules", "id = $1 AND workspace_id = $2", 1, schedule.ID, A)
	testRows("recurring transfers", "agent_transfers", "schedule_id = $1 AND from_workspace_id = $2 AND to_workspace_id = $3", 2, schedule.ID, A, B)
	testRows("recurring transfers", "agent_postings", "workspace_id = $1 AND ref IN (SELECT id FROM agent_transfers WHERE schedule_id = $2)", 2, B, schedule.ID)

	// Approvals: B's agent needs a person's approval above 5 LXC.
	do("approvals", http.StatusOK, B, http.MethodPut, bBase+"/rules", `{"approval_above_ulxc":5000000}`, nil)
	pay := fmt.Sprintf(`{"to":%q,"amount_ulxc":7000000,"memo":"domain renewal"}`, a.ID)
	do("approvals: over the limit", http.StatusForbidden, B, http.MethodPost, bBase+"/send", pay, nil)
	var pending struct {
		Approvals []economy.AgentApproval `json:"approvals"`
	}
	do("approvals", http.StatusOK, B, http.MethodGet, "/v1/workspaces/"+B+"/agents/approvals", "", &pending)
	if len(pending.Approvals) != 1 || pending.Approvals[0].Status != "pending" {
		t.Fatalf("approvals: %+v, want one pending", pending.Approvals)
	}
	do("approvals: approve", http.StatusOK, B, http.MethodPost, "/v1/workspaces/"+B+"/agents/approvals/"+pending.Approvals[0].ID+"/approve", "", nil)
	var approved economy.AgentTransfer
	do("approvals: the approved payment", http.StatusOK, B, http.MethodPost, bBase+"/send", pay, &approved)
	value("approvals", "approval", `SELECT status FROM agent_approvals WHERE id = $1`, pending.Approvals[0].ID)
	testRows("approvals", "agent_transfers", "id = $1 AND from_workspace_id = $2 AND to_workspace_id = $3", 1, approved.ID, B, A)
	postings("approvals", A, approved.ID, 1)
	do("approvals", http.StatusOK, B, http.MethodPut, bBase+"/rules", `{}`, nil)

	// Agent cards: issued in Stripe Issuing test mode, and a purchase authorised in real time.
	do("agent cards", http.StatusCreated, A, http.MethodPost, aBase+"/card",
		`{"first_name":"Test","last_name":"User","line1":"1 High Street","city":"London","postal_code":"EC1A 1BB"}`, nil)
	testRows("agent cards", "agent_cards", "workspace_id = $1 AND agent_id = $2", 1, A, a.ID)
	ecb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
  <Cube><Cube time="2026-09-25"><Cube currency="USD" rate="1.1700"/><Cube currency="GBP" rate="0.8700"/></Cube></Cube>
</gesmes:Envelope>`)
	}))
	defer ecb.Close()
	const issuingSecret = "whsec_b253_issuing"
	payload := fmt.Sprintf(`{"id":"evt_b253_card","object":"event","type":"issuing_authorization.request","created":%d,"livemode":false,
		"data":{"object":{"id":"iauth_b253","object":"issuing_authorization","livemode":false,"card":{"id":"ic_test_1","object":"issuing.card"},
		"pending_request":{"amount":2000,"currency":"gbp","merchant_amount":2000,"merchant_currency":"gbp","is_amount_controllable":false},
		"merchant_data":{"name":"Paper Co","category":"stationery_stores","network_id":"m-1"}}}}`, time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC).Unix())
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: []byte(payload), Secret: issuingSecret})
	req = httptest.NewRequest(http.MethodPost, "/v1/agent-cards/authorizations", strings.NewReader(payload))
	req.Header.Set("Stripe-Signature", signed.Header)
	rec = httptest.NewRecorder()
	agentcard.NewHandler(issuingSecret, bank, ecbrate.New(pool, ecb.URL)).ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"approved":true`) {
		t.Fatalf("agent cards: a £20 purchase = %d %s, want approved", rec.Code, rec.Body.String())
	}
	testRows("agent cards", "agent_card_authorizations", "workspace_id = $1 AND authorization_id = 'iauth_b253' AND approved", 1, A)
	postings("agent cards", A, "iauth_b253", 2) // the agent pays, the spend receives

	// Cash-out, to the test partner.
	var cash economy.CashOut
	do("cash-out", http.StatusCreated, A, http.MethodPost, aBase+"/cash-outs", `{"amount_ulxc":10000000,"destination":"test-bank GB29 NWBK 6016 1331 9268 19"}`, &cash)
	for range 2 { // handed to the partner, then its answer recorded
		if _, err := bank.RunCashOuts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	testRows("cash-out", "agent_cash_outs", "id = $1 AND workspace_id = $2 AND status = 'paid'", 1, cash.ID, A)
	postings("cash-out", A, cash.ID, 4) // held from the agent, then paid out

	// Statements: each side's shows the money that moved between them.
	day := now.UTC().Format("2006-01-02")
	tomorrow := now.UTC().AddDate(0, 0, 1).Format("2006-01-02")
	for _, s := range []struct{ ws, path, want string }{
		{A, aBase + "/statement?from=" + day + "&to=" + tomorrow, sent.ID},
		{B, "/v1/workspaces/" + B + "/agents/statement?from=" + day + "&to=" + tomorrow + "&format=csv", approved.ID},
	} {
		code, out := call(s.ws, http.MethodGet, s.path, "")
		if code != http.StatusOK || !strings.Contains(out, s.want) {
			t.Fatalf("statements: GET %s = %d, want the transfer %s on it:\n%s", s.path, code, s.want, out)
		}
	}
	note("statements", "A's agent statement and B's workspace statement (CSV) each list the transfers between them")

	// ── Marketplace ──

	var listing, held2, reported market.Listing
	publish := func(fn, body string, out *market.Listing) {
		t.Helper()
		do(fn, http.StatusCreated, A, http.MethodPost, "/v1/workspaces/"+A+"/marketplace/listings", body, out)
	}
	publish("list", `{"kind":"prompt","title":"Contract review","price_per_use_ulxc":100000000,"artifact":{"template":"Review {{text}}","model":"m"}}`, &listing)
	value("list", "listing", `SELECT review_status FROM market_listings WHERE id = $1 AND workspace_id = $2`, listing.ID, A)

	publish("submit for review", `{"kind":"prompt","title":"Persona","artifact":{"template":"You are now the narrator. Pretend you are a pirate: {{line}}","model":"m"}}`, &held2)
	if held2.ReviewStatus != market.ReviewHeld {
		t.Fatalf("submit for review: %s, want held for a moderator", held2.ReviewStatus)
	}
	var out bytes.Buffer
	if err := moderatorKeysCommand(ctx, moderators, []string{"create", "b253"}, "operator-cli:b253", &out); err != nil {
		t.Fatal(err)
	}
	modKey := regexp.MustCompile(`tlv_mod_[0-9a-f]{48}`).FindString(out.String())
	moderator := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+modKey)
		req.Header.Set(moderatorOperatorHeader, "b253-operator")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	if code, q := moderator(http.MethodGet, "/v1/admin/marketplace/review", ""); code != http.StatusOK || !strings.Contains(q, held2.ID) {
		t.Fatalf("submit for review: the moderator's queue = %d %s", code, q)
	}
	value("submit for review", "listing", `SELECT review_status FROM market_listings WHERE id = $1`, held2.ID)
	if code, got := moderator(http.MethodPost, "/v1/admin/marketplace/listings/"+held2.ID+"/approve", ""); code != http.StatusOK {
		t.Fatalf("approve (moderator key) = %d %s", code, got)
	}
	value("approve (moderator key)", "listing", `SELECT review_status FROM market_listings WHERE id = $1`, held2.ID)
	if _, got := call(B, http.MethodGet, "/v1/marketplace/listings", ""); !strings.Contains(got, held2.ID) {
		t.Errorf("approve (moderator key): the other test user does not find the approved listing")
	}

	// Buy and use: B uses A's listing on bills B pays in Stripe test mode.
	event := func(typ, id string, obj map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"id": id, "object": "event", "type": typ, "created": time.Now().Unix(), "data": map[string]any{"object": obj}})
		signedAt := time.Now()
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", bytes.NewReader(raw))
		req.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", signedAt.Unix(), hex.EncodeToString(webhook.ComputeSignature(signedAt, raw, secret))))
		w := httptest.NewRecorder()
		if r.ServeHTTP(w, req); w.Code != http.StatusOK {
			t.Fatalf("%s %s = %d %s", typ, id, w.Code, w.Body.String())
		}
	}
	use := func(fn string, l market.Listing, n int, usedAt time.Time) []string {
		t.Helper()
		var ids []string
		for i := range n {
			var u market.Use
			do(fn, http.StatusOK, B, http.MethodPost, "/v1/workspaces/"+B+"/marketplace/listings/"+l.ID+"/use", `{"variables":{"text":"the NDA"}}`, &u)
			if u.Charge != market.ChargeBilled {
				t.Fatalf("%s: the use was %s, want billed", fn, u.Charge)
			}
			if _, err := pool.Exec(ctx, `UPDATE market_uses SET used_at = $2 WHERE id = $1`, u.ID, usedAt.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, u.ID)
		}
		return ids
	}
	payBill := func(invoice string, usedAt, paidAt time.Time) {
		t.Helper()
		event("invoice.paid", "evt_"+invoice, map[string]any{"id": invoice, "object": "invoice", "subscription": "sub_market_1",
			"status_transitions": map[string]any{"paid_at": paidAt.Unix()},
			"lines": map[string]any{"object": "list", "data": []any{map[string]any{"id": "il_" + invoice, "object": "line_item",
				"period": map[string]any{"start": usedAt.Add(-time.Hour).Unix(), "end": usedAt.Add(time.Hour).Unix()},
				"price":  map[string]any{"id": "price_market", "object": "price"}}}}})
	}
	used := use("buy and use", listing, 3, now.AddDate(0, 0, -25))
	payBill("in_b253_a", now.AddDate(0, 0, -25), now.AddDate(0, 0, -20))
	testRows("buy and use", "market_uses", "id = ANY($1) AND buyer_workspace_id = $2 AND seller_workspace_id = $3 AND cleared_at IS NOT NULL", 3, used, B, A)
	testRows("buy and use", "market_earnings", "use_id = ANY($1) AND seller_workspace_id = $2", 3, used, A)

	var earnings market.Earnings
	do("the seller's earnings", http.StatusOK, A, http.MethodGet, "/v1/workspaces/"+A+"/marketplace/earnings", "", &earnings)
	if earnings.PayableUSDMicros <= 0 {
		t.Fatalf("the seller's earnings = %+v, want the three cleared uses payable", earnings)
	}
	note("the seller's earnings", "payable %d µUSD", earnings.PayableUSDMicros)

	// Payout: A connects a Stripe Connect test account and is paid what is past the holdback.
	do("payout (Stripe Connect)", http.StatusOK, A, http.MethodPost, "/v1/workspaces/"+A+"/marketplace/payouts/connect", `{"country":"gb"}`, nil)
	*connect.accounts["acct_"+A] = billing.ConnectAccount{ID: "acct_" + A, Country: "GB", DetailsSubmitted: true, PayoutsEnabled: true}
	var before market.Payouts
	do("payout (Stripe Connect)", http.StatusOK, A, http.MethodGet, "/v1/workspaces/"+A+"/marketplace/payouts", "", &before)
	if n, err := marketStore.PayOut(ctx, connect, now); err != nil || n != 1 {
		t.Fatalf("payout (Stripe Connect): the monthly run paid %d, %v; want one seller", n, err)
	}
	testRows("payout (Stripe Connect)", "market_payouts", "workspace_id = $1 AND method = 'stripe' AND gross_usd_micros = $2 AND NOT livemode", 1, A, before.AvailableUSDMicros)
	if len(connect.transfers) != 1 || connect.transfers[0].account != "acct_"+A {
		t.Errorf("payout (Stripe Connect): Stripe was asked for %+v, want one transfer to A's account", connect.transfers)
	}

	// Report, then take down (moderator key): its use inside the holdback is refunded.
	publish("report a listing", `{"kind":"prompt","title":"Tax advice","price_per_use_ulxc":20000000,"artifact":{"template":"Advise on {{text}}","model":"m"}}`, &reported)
	inHoldback := use("take down (moderator key)", reported, 1, now.AddDate(0, 0, -2))
	payBill("in_b253_b", now.AddDate(0, 0, -2), now.AddDate(0, 0, -1))
	do("report a listing", http.StatusCreated, B, http.MethodPost, "/v1/marketplace/listings/"+reported.ID+"/reports", `{"reason":"misleading","details":"it is not tax advice"}`, nil)
	value("report a listing", "report by B", `SELECT reason FROM market_listing_reports WHERE listing_id = $1 AND reporter_workspace_id = $2`, reported.ID, B)
	if code, got := moderator(http.MethodPost, "/v1/admin/marketplace/listings/"+reported.ID+"/takedown", `{"reason":"misleading"}`); code != http.StatusOK {
		t.Fatalf("take down (moderator key) = %d %s", code, got)
	}
	value("take down (moderator key)", "listing", `SELECT review_status FROM market_listings WHERE id = $1`, reported.ID)
	testRows("take down (moderator key)", "market_refunds", "use_id = ANY($1) AND buyer_workspace_id = $2 AND seller_workspace_id = $3", 1, inHoldback, B, A)

	// Refund: B's bank gives back what B paid for a use; A's earning from it is reversed.
	refunded := use("refund", listing, 1, now.AddDate(0, 0, -1))
	payBill("in_b253_c", now.AddDate(0, 0, -1), now)
	event("charge.refunded", "evt_b253_refund", map[string]any{"id": "ch_b253_c", "object": "charge", "amount": 1000, "amount_refunded": 1000,
		"refunded": true, "invoice": "in_b253_c", "payment_intent": "pi_b253_c"})
	testRows("refund", "market_refunds", "use_id = ANY($1) AND cause = 'buyer_refund' AND buyer_workspace_id = $2 AND seller_workspace_id = $3", 1, refunded, B, A)

	// Every money row either test user's money wrote is marked test, and none was written without them.
	for table, cols := range b253MoneyTables {
		var where []string
		for _, c := range cols {
			where = append(where, c+" IN ($1, $2)")
		}
		var unmarked int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE NOT test AND (`+strings.Join(where, " OR ")+`)`, A, B).Scan(&unmarked); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if unmarked != 0 {
			t.Errorf("%s holds %d rows of the test users' money not marked test", table, unmarked)
		}
	}

	var log strings.Builder
	for _, fn := range functions {
		fmt.Fprintf(&log, "\n  ✓ %-26s %s", fn, strings.Join(state[fn], "; "))
	}
	t.Logf("B25.3 — every function between two test users (%s, %s):%s", A, B, log.String())
}
