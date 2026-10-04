package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
)

// B28.299 — GIVE BACK A RECEIVED TRANSFER, FROM THE SCREEN.
//
// The wallet screen lists GET …/agents/{id}/transfers and shows "Give back" on a row Lens marks refundable;
// pressing it is the owner's POST …/transfers/{id}/refund. Company A's agent sends B's agent 30 LXC: B's row is
// refundable, A's is not. B gives it back: the refund is one entry of two postings — −30 on B's agent, +30 on
// A's — both balances move by it, and B's list then names the refund and offers no second one.
func TestB28299_GiveBackAReceivedTransferFromTheScreen(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetOwnerVerifier(earnverify.New(false))
	const lxc = int64(1_000_000)

	agent := func(ws, handle string, credits int64) economy.Agent {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
		a, err := bank.CreateAgent(ctx, ws, handle, "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bank.SetAgentHandle(ctx, ws, a.ID, handle); err != nil {
			t.Fatal(err)
		}
		if credits > 0 {
			if _, err := bank.CreditLXC(ctx, ws, credits, "stripe top-up", map[string]interface{}{"funding": economy.FundingTest}); err != nil {
				t.Fatal(err)
			}
			if _, err := bank.FundAgent(ctx, ws, a.ID, credits); err != nil {
				t.Fatal(err)
			}
		}
		return a
	}
	const A, B = "b28299-sender-co", "b28299-receiver-co"
	sender := agent(A, "b28299-sender", 100*lxc)
	receiver := agent(B, "b28299-receiver", 0)

	r := chi.NewRouter()
	mountAgentTransferRoutes(r, bank)
	// Each call is the workspace owner's, as the BFF makes it.
	call := func(ws, method, path, body string, want int, out any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, w.Code, w.Body.String(), want)
		}
		if out != nil {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
		}
	}
	type row struct {
		ID         string `json:"id"`
		EntryID    string `json:"entry_id"`
		RefundOf   string `json:"refund_of"`
		RefundedBy string `json:"refunded_by"`
		Refundable bool   `json:"refundable"`
	}
	list := func(ws, agentID string) map[string]row {
		t.Helper()
		var out struct {
			Transfers []row `json:"transfers"`
		}
		call(ws, http.MethodGet, "/v1/workspaces/"+ws+"/agents/"+agentID+"/transfers", "", http.StatusOK, &out)
		m := map[string]row{}
		for _, tr := range out.Transfers {
			m[tr.ID] = tr
		}
		return m
	}
	balance := func(ws string) int64 {
		t.Helper()
		b, err := bank.AgentBook(ctx, ws)
		if err != nil || len(b.Agents) != 1 {
			t.Fatalf("%s: book = %+v, %v", ws, b, err)
		}
		return b.Agents[0].BalanceULXC
	}

	var sent row
	call(A, http.MethodPost, "/v1/workspaces/"+A+"/agents/"+sender.ID+"/send", `{"to":"@b28299-receiver","amount_ulxc":30000000,"memo":"deposit"}`, http.StatusOK, &sent)
	if got := list(B, receiver.ID)[sent.ID]; !got.Refundable || got.RefundedBy != "" {
		t.Fatalf("the receiver's row = %+v, want refundable and not yet given back", got)
	}
	if got := list(A, sender.ID)[sent.ID]; got.Refundable {
		t.Fatalf("the sender's row = %+v, want not refundable — only the receiver gives back", got)
	}
	senderBefore, receiverBefore := balance(A), balance(B)

	var back row
	call(B, http.MethodPost, "/v1/workspaces/"+B+"/transfers/"+sent.ID+"/refund", "", http.StatusOK, &back)
	if back.RefundOf != sent.ID || back.EntryID == "" {
		t.Fatalf("refund = %+v, want a transfer giving back %s", back, sent.ID)
	}

	// The ledger: the refund is one entry of two postings, −30 on the receiver's agent and +30 on the sender's.
	rows, err := pool.Query(ctx, `SELECT account, amount_ulxc FROM agent_postings WHERE entry_id::text = $1 ORDER BY amount_ulxc`, back.EntryID)
	if err != nil {
		t.Fatal(err)
	}
	type posting struct {
		account string
		amount  int64
	}
	var postings []posting
	for rows.Next() {
		var p posting
		if err := rows.Scan(&p.account, &p.amount); err != nil {
			t.Fatal(err)
		}
		postings = append(postings, p)
	}
	if rows.Err() != nil || len(postings) != 2 ||
		postings[0] != (posting{"agent:" + receiver.ID, -30 * lxc}) || postings[1] != (posting{"agent:" + sender.ID, 30 * lxc}) {
		t.Fatalf("the refund's postings = %+v (%v), want −30 LXC on agent:%s and +30 LXC on agent:%s", postings, rows.Err(), receiver.ID, sender.ID)
	}
	if s, r := balance(A), balance(B); s != senderBefore+30*lxc || r != receiverBefore-30*lxc {
		t.Fatalf("balances after the refund: sender %d (was %d), receiver %d (was %d); want each moved by 30 LXC", s, senderBefore, r, receiverBefore)
	}

	// The screen now shows it given back, with no second button, and the refund itself is not refundable.
	after := list(B, receiver.ID)
	if got := after[sent.ID]; got.Refundable || got.RefundedBy != back.ID {
		t.Fatalf("the receiver's row after = %+v, want refunded_by %s and not refundable", got, back.ID)
	}
	if got := after[back.ID]; got.Refundable {
		t.Fatalf("the refund's own row = %+v, want not refundable", got)
	}
	call(B, http.MethodPost, "/v1/workspaces/"+B+"/transfers/"+sent.ID+"/refund", "", http.StatusConflict, nil)
}
