package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// B22.5 — on the migrated schema, with test money: company A's agent lends company B's agent 100 LXC at 10%
// in three daily instalments; B accepts and the principal is paid out; two instalments are taken with their
// interest; the third cannot be, so the loan goes late, and missed again (with the late fee) it defaults. Both
// statements carry the loan. A private user on either side is refused as class RED, and an offer of live money
// with no clearance is refused as class AMBER.
func TestCompanyLoans_LendRepayLateDefaultAndWhoMayBorrow(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	s.SetOwnerVerifier(earnVerified{})
	const lxc = int64(1_000_000)
	for ws, company := range map[string]bool{"co-a": true, "co-b": true, "co-live": true, "person": false} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified, company) VALUES ($1, $1, $1, true, $2)`, ws, company); err != nil {
			t.Fatal(err)
		}
	}
	agent := func(ws, handle, funding string, credits int64) Agent {
		t.Helper()
		a, err := s.CreateAgent(ctx, ws, handle, "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetAgentHandle(ctx, ws, a.ID, handle); err != nil {
			t.Fatal(err)
		}
		if credits > 0 {
			if _, err := s.CreditLXC(ctx, ws, credits, "stripe top-up", map[string]interface{}{"funding": funding}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.FundAgent(ctx, ws, a.ID, credits); err != nil {
				t.Fatal(err)
			}
		}
		return a
	}
	lender := agent("co-a", "lender", FundingTest, 500*lxc)
	borrower := agent("co-b", "borrower", "", 0)
	person := agent("person", "person", FundingTest, 500*lxc)
	liveLender := agent("co-live", "live-lender", FundingLive, 500*lxc)
	terms := LoanTerms{PrincipalULXC: 100 * lxc, InterestBPS: 1000, Instalments: 3, Every: "day", LateFeeULXC: 2 * lxc, Memo: "working capital"}

	// Who may lend and borrow.
	for _, try := range []struct{ ws, agent, to string }{{"co-a", lender.ID, "@person"}, {"person", person.ID, "@borrower"}} {
		if _, err := s.OfferLoan(ctx, try.ws, try.agent, try.to, terms); !errors.Is(err, ErrLoanCompaniesOnly) || !strings.Contains(err.Error(), "class RED") {
			t.Fatalf("%s offering to %s = %v, want refused as class RED", try.ws, try.to, err)
		}
	}
	if _, err := s.OfferLoan(ctx, "co-live", liveLender.ID, "@borrower", terms); !errors.Is(err, ErrCapabilityNotCleared) || !strings.Contains(err.Error(), "class AMBER") {
		t.Fatalf("an offer of live money with no clearance = %v, want refused as class AMBER", err)
	}

	// A offers, B accepts: the principal is paid out.
	offer, err := s.OfferLoan(ctx, "co-a", lender.ID, "@borrower", terms)
	if err != nil {
		t.Fatal(err)
	}
	loan, err := s.AnswerLoan(ctx, "co-b", offer.ID, true)
	if err != nil || loan.Status != "active" || loan.NextDueAt == nil {
		t.Fatalf("accept = %+v, %v", loan, err)
	}
	holds := func(ws string, a Agent) int64 {
		t.Helper()
		var bal int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings WHERE workspace_id = $1 AND account = $2`,
			ws, agentAccount(a.ID)).Scan(&bal); err != nil {
			t.Fatal(err)
		}
		return bal
	}
	if holds("co-b", borrower) != 100*lxc {
		t.Fatalf("the borrower holds %d, want the 100 LXC principal", holds("co-b", borrower))
	}

	// Four daily ticks: instalments 1 and 2 are taken; 3 (36.67 LXC) is more than the 26.67 LXC left: late,
	// then default.
	for i := 0; i < 4; i++ {
		if loan, err = s.GetLoan(ctx, "co-a", offer.ID); err != nil || loan.NextDueAt == nil {
			t.Fatalf("tick %d: loan %+v, %v", i, loan, err)
		}
		if _, err := s.RunLoanRepayments(ctx, loan.NextDueAt.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if loan, err = s.GetLoan(ctx, "co-b", offer.ID); err != nil || loan.Status != "defaulted" || loan.PaidInstalments != 2 || loan.NextDueAt != nil {
		t.Fatalf("after four ticks = %+v, %v; want defaulted with 2 instalments paid", loan, err)
	}
	var kinds []string
	var interest int64
	for _, e := range loan.Events {
		kinds = append(kinds, e.Kind)
		if e.Kind == "instalment" {
			interest += e.InterestULXC
		}
	}
	if got := strings.Join(kinds, " "); got != "payout instalment instalment missed late missed defaulted" || interest != 6_666_666 {
		t.Fatalf("events %q with %d µLXC interest paid; want payout, two instalments with 6.666666 LXC interest, late, default", got, interest)
	}
	if loan.Events[5].LateFeeULXC != 2*lxc {
		t.Errorf("the retried instalment asked %d µLXC late fee, want 2 LXC", loan.Events[5].LateFeeULXC)
	}
	paid := 2 * (33_333_333 + 3_333_333)
	if holds("co-a", lender) != 400*lxc+int64(paid) || holds("co-b", borrower) != 100*lxc-int64(paid) {
		t.Errorf("lender holds %d, borrower %d; want %d and %d", holds("co-a", lender), holds("co-b", borrower), 400*lxc+int64(paid), 100*lxc-int64(paid))
	}
	// A loan's money cannot come back as a plain refund.
	if _, err := s.RefundTransfer(ctx, "co-b", loan.Events[0].TransferID); !errors.Is(err, ErrTransferNotFound) {
		t.Errorf("refunding the payout = %v, want refused", err)
	}

	// Both statements carry the loan and its events.
	for _, ws := range []string{"co-a", "co-b"} {
		st, err := s.WorkspaceAgentStatement(ctx, ws, time.Now().Add(-time.Hour), time.Now().AddDate(0, 0, 10))
		if err != nil || len(st.Loans) != 1 || st.Loans[0].ID != offer.ID || len(st.Loans[0].Events) != 7 {
			t.Errorf("%s's statement loans = %+v, %v; want the loan with its 7 events", ws, st.Loans, err)
		}
	}
}
