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
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/agentcard"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B19.25 — after a purchase is approved, Stripe's Issuing events on the regular webhook settle its hold: a
// reversal credits the agent back exactly, a capture above or below the authorisation adjusts it by the
// difference, and a refund credits it — each a ledger row, on the migrated schema.
func TestAgentCardSettlements_ReversalCaptureAndRefundSettleTheAgentsBalance(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-card-settle"
	for _, q := range []string{
		`INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ('ws-card-settle', 2000000000, 2000000000)`,
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ('ws-card-settle', 'ws-card-settle', 'ws-card-settle')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentCardRoutes(r, store, &fakeCardIssuer{})
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-nicolai", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	base := "/v1/workspaces/" + ws + "/agents"
	code, body := call(http.MethodPost, base, `{"name":"buyer"}`)
	if code != http.StatusCreated {
		t.Fatalf("create agent = %d %s", code, body)
	}
	var agent economy.Agent
	_ = json.Unmarshal([]byte(body), &agent)
	if code, body := call(http.MethodPost, base+"/"+agent.ID+"/fund", `{"amount_ulxc":1000000000}`); code != http.StatusOK {
		t.Fatalf("fund = %d %s", code, body)
	}
	holder := `{"first_name":"Nicolai","last_name":"Gaborac","line1":"1 High Street","city":"London","postal_code":"EC1A 1BB"}`
	if code, body := call(http.MethodPost, base+"/"+agent.ID+"/card", holder); code != http.StatusCreated {
		t.Fatalf("issue card = %d %s", code, body)
	}

	ecb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
  <Cube><Cube time="2026-09-25"><Cube currency="USD" rate="1.1700"/><Cube currency="GBP" rate="0.8700"/></Cube></Cube>
</gesmes:Envelope>`)
	}))
	defer ecb.Close()
	at := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	send := func(h http.Handler, path, secret, payload string) *httptest.ResponseRecorder {
		t.Helper()
		signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: []byte(payload), Secret: secret})
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
		req.Header.Set("Stripe-Signature", signed.Header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	// Three £20.00 purchases approved in real time, each holding 268,965,520 µLXC (at 1.17 USD, 0.87 GBP per EUR).
	realtime := agentcard.NewHandler("whsec_realtime", store, ecbrate.New(pool, ecb.URL))
	for _, id := range []string{"rev", "up", "down"} {
		w := send(realtime, "/v1/agent-cards/authorizations", "whsec_realtime", fmt.Sprintf(`{"id":"evt_req_%s","object":"event",
			"type":"issuing_authorization.request","created":%d,"livemode":false,"data":{"object":{"id":"iauth_%s",
			"object":"issuing.authorization","livemode":false,"card":{"id":"ic_test_1","object":"issuing.card"},
			"pending_request":{"amount":2000,"currency":"gbp","merchant_amount":2000,"merchant_currency":"gbp"},
			"merchant_data":{"name":"Paper Co","category":"stationery_stores","network_id":"m-1"}}}}`, id, at.Unix(), id))
		if !strings.Contains(w.Body.String(), `"approved":true`) {
			t.Fatalf("£20 purchase %s: %s", id, w.Body.String())
		}
	}

	// The regular webhook, with the Issuing events Stripe sends after each decision.
	const secret = "whsec_billing"
	hook := http.HandlerFunc(billing.New(pool, store, nil, secret).WithAgentCards(store).HandleWebhook)
	updated := func(evt, auth, status string, held int64, livemode bool) {
		t.Helper()
		w := send(hook, "/v1/billing/webhook", secret, fmt.Sprintf(`{"id":%q,"object":"event","type":"issuing_authorization.updated",
			"created":%d,"livemode":%t,"data":{"object":{"id":%q,"object":"issuing.authorization","livemode":%t,"status":%q,
			"amount":%d,"currency":"gbp","card":{"id":"ic_test_1","object":"issuing.card"}}}}`, evt, at.Unix(), livemode, auth, livemode, status, held))
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", evt, w.Code, w.Body.String())
		}
	}
	transaction := func(evt, txn, kind, auth string, amount int64) {
		t.Helper()
		w := send(hook, "/v1/billing/webhook", secret, fmt.Sprintf(`{"id":%q,"object":"event","type":"issuing_transaction.created",
			"created":%d,"livemode":false,"data":{"object":{"id":%q,"object":"issuing.transaction","livemode":false,"type":%q,
			"amount":%d,"currency":"gbp","card":"ic_test_1","authorization":%q}}}`, evt, at.Unix(), txn, kind, amount, auth))
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", evt, w.Code, w.Body.String())
		}
	}
	updated("evt_rev", "iauth_rev", "reversed", 0, false)                   // reversed: +268,965,520, exactly what it debited
	transaction("evt_cap_up", "ipi_up", "capture", "iauth_up", -2300)       // £23 on £20 held: −£3 = −40,344,830
	updated("evt_close_up", "iauth_up", "closed", 0, false)                 // nothing left held: no row
	updated("evt_close_down", "iauth_down", "closed", 0, false)             // closed before its capture: +268,965,520
	transaction("evt_cap_down", "ipi_down", "capture", "iauth_down", -1500) // £15 captured: −201,724,140 (net +£5)
	transaction("evt_refund", "ipi_refund", "refund", "iauth_up", 500)      // £5 back: +67,241,380
	transaction("evt_refund", "ipi_refund", "refund", "iauth_up", 500)      // Stripe sent it again: nothing more
	updated("evt_live", "iauth_rev", "reversed", 0, true)                   // live mode: nothing

	var got []int64
	res, err := pool.Query(ctx, `SELECT amount FROM lxc_ledger WHERE workspace_id = $1 AND type = 'agent_card'
		AND metadata ? 'settlement' ORDER BY created_at`, ws)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
		var a int64
		if err := res.Scan(&a); err != nil {
			t.Fatal(err)
		}
		got = append(got, a)
	}
	res.Close()
	want := []int64{268_965_520, -40_344_830, 268_965_520, -201_724_140, 67_241_380}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("settlement ledger rows = %v, want %v", got, want)
	}
	var settlements int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_card_settlements WHERE agent_id = $1`, agent.ID).Scan(&settlements); err != nil {
		t.Fatal(err)
	}
	if settlements != 5 {
		t.Errorf("agent_card_settlements rows = %d, want 5 (the close with nothing held, the replay and live mode record nothing)", settlements)
	}
	// The agent paid £23 − £5 and £15: 1,000,000,000 − 309,310,350 + 67,241,380 − 201,724,140.
	book, err := store.AgentBook(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	var wsBal int64
	if err := pool.QueryRow(ctx, `SELECT balance FROM lxc_balances WHERE workspace_id = $1`, ws).Scan(&wsBal); err != nil {
		t.Fatal(err)
	}
	if b := book.Agents[0]; b.BalanceULXC != 556_206_890 || wsBal != 1_556_206_890 {
		t.Errorf("the agent holds %d and the workspace %d; want 556,206,890 and 1,556,206,890", b.BalanceULXC, wsBal)
	}
}
