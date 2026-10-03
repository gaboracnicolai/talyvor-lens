package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
)

// B26.14 — A LOAN'S TRANSFERS NAME THE LOAN THEY BELONG TO.
//
// Company A lends company B's agent 100 LXC in two instalments; B accepts and both instalments are taken. On
// each side, GET …/agents/{id}/transfers lists the payout and both repayments with the loan's id — the same
// loan_id the agent_transfers rows carry.
func TestB2614_ALoansTransfersNameTheLoan(t *testing.T) {
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
		if _, err := bank.CreditLXC(ctx, ws, credits, "stripe top-up", map[string]interface{}{"funding": economy.FundingTest}); err != nil {
			t.Fatal(err)
		}
		if _, err := bank.FundAgent(ctx, ws, a.ID, credits); err != nil {
			t.Fatal(err)
		}
		return a
	}
	lender := agent("b2614-lender-co", "b2614-lender", 500*lxc)
	borrower := agent("b2614-borrower-co", "b2614-borrower", 50*lxc) // enough over the principal for the interest

	offer, err := bank.OfferLoan(ctx, "b2614-lender-co", lender.ID, "@b2614-borrower",
		economy.LoanTerms{PrincipalULXC: 100 * lxc, InterestBPS: 1000, Instalments: 2, Every: "day", Memo: "working capital"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bank.AnswerLoan(ctx, "b2614-borrower-co", offer.ID, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		loan, err := bank.GetLoan(ctx, "b2614-lender-co", offer.ID)
		if err != nil || loan.NextDueAt == nil {
			t.Fatalf("instalment %d: loan %+v, %v", i+1, loan, err)
		}
		if _, err := bank.RunLoanRepayments(ctx, loan.NextDueAt.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if loan, err := bank.GetLoan(ctx, "b2614-lender-co", offer.ID); err != nil || loan.PaidInstalments != 2 {
		t.Fatalf("after two ticks = %+v, %v; want both instalments paid", loan, err)
	}

	// The rows: the payout and two repayments, each carrying the loan's id.
	rows, err := pool.Query(ctx, `SELECT id FROM agent_transfers WHERE loan_id = $1`, offer.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		want[id] = true
	}
	if rows.Err() != nil || len(want) != 3 {
		t.Fatalf("agent_transfers rows naming the loan = %d (%v), want the payout and two repayments", len(want), rows.Err())
	}

	r := chi.NewRouter()
	mountAgentTransferRoutes(r, bank)
	for _, side := range []struct{ ws, agent string }{{"b2614-lender-co", lender.ID}, {"b2614-borrower-co", borrower.ID}} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+side.ws+"/agents/"+side.agent+"/transfers", nil))
		var out struct {
			Transfers []struct {
				ID     string `json:"id"`
				LoanID string `json:"loan_id"`
			} `json:"transfers"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s: GET transfers = %d %s", side.ws, w.Code, w.Body.String())
		}
		got := map[string]bool{}
		for _, tr := range out.Transfers {
			if want[tr.ID] {
				if tr.LoanID != offer.ID {
					t.Errorf("%s: transfer %s lists loan_id %q, want %q", side.ws, tr.ID, tr.LoanID, offer.ID)
				}
				got[tr.ID] = true
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: lists %d of the loan's 3 transfers", side.ws, len(got))
		}
	}
}
