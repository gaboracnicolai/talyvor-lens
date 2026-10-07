package partners

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CapitalPartner funds credit: every credit draw, advance or loan is the partner's money, never Talyvor's — the
// capabilities b2b_credit, seller_advances and lending_marketplace. Credit is for companies only.
type CapitalPartner interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// Offer asks the partner what it would lend a company. Idempotent on req.ID.
	Offer(ctx context.Context, req CapitalRequest) (CapitalOffer, error)
	// Fund draws an offer down into an account. Idempotent on req.ID.
	Fund(ctx context.Context, req Funding) (Result, error)
	// Repay pays a funding back, in whole or in part. Idempotent on req.ID.
	Repay(ctx context.Context, req Repayment) (Result, error)
	// Status says where an offer, a funding or a repayment stands now.
	Status(ctx context.Context, ref string) (Result, error)
}

// CapitalRequest asks for credit for a company.
type CapitalRequest struct {
	ID            string `json:"id"` // the caller's id for the offer
	Company       string `json:"company"`
	CompanyNumber string `json:"company_number"`
	Amount        Money  `json:"amount"`
	TermDays      int    `json:"term_days"`
}

// CapitalOffer is what the partner would lend: the amount, its fee, and when it is to be repaid.
type CapitalOffer struct {
	Result
	Amount  Money     `json:"amount"`
	Fee     Money     `json:"fee"`
	RepayBy time.Time `json:"repay_by"`
}

// Funding draws an offer down.
type Funding struct {
	ID         string `json:"id"` // the caller's id for the funding
	OfferRef   string `json:"offer_ref"`
	AccountRef string `json:"account_ref"` // where the money goes
}

// Repayment pays a funding back.
type Repayment struct {
	ID         string `json:"id"` // the caller's id for the repayment
	FundingRef string `json:"funding_ref"`
	Amount     Money  `json:"amount"`
}

// TestCapitalPartner is test mode for credit: it lends nothing and charges nothing. It offers a company exactly
// what it asks for over the term it asks, declines an amount ending in 13 and keeps one ending in 15 under review;
// an offer ending in 14 is funded and then recalled. Repayments answer by their own amount (see the package
// comment).
type TestCapitalPartner struct {
	book testBook
}

// Name is "test".
func (*TestCapitalPartner) Name() string { return "test" }

// Offer offers what was asked, at no fee.
func (p *TestCapitalPartner) Offer(_ context.Context, req CapitalRequest) (CapitalOffer, error) {
	if err := checkMoney(req.Amount); err != nil {
		return CapitalOffer{}, err
	}
	if strings.TrimSpace(req.Company) == "" || strings.TrimSpace(req.CompanyNumber) == "" {
		return CapitalOffer{}, fmt.Errorf("%w: credit is for companies only: an offer names the company and its number", ErrInvalid)
	}
	if req.TermDays <= 0 {
		return CapitalOffer{}, fmt.Errorf("%w: an offer has a term of at least a day", ErrInvalid)
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	op, _, err := p.book.record("offer", req.ID, req, func(string) (Result, Status) {
		now, later, detail := testOutcome(req.Amount.Minor)
		if later == StatusReturned { // the offer stands; it is its funding that comes back
			return Result{Status: StatusCompleted}, StatusCompleted
		}
		return Result{Status: now, Detail: detail}, later
	})
	if err != nil {
		return CapitalOffer{}, err
	}
	return CapitalOffer{Result: op.result, Amount: req.Amount, Fee: Money{0, req.Amount.Currency},
		RepayBy: op.at.AddDate(0, 0, req.TermDays)}, nil
}

// Fund lends nothing: an offer that stands is funded, and one of an amount ending in 14 is recalled after.
func (p *TestCapitalPartner) Fund(_ context.Context, req Funding) (Result, error) {
	if strings.TrimSpace(req.AccountRef) == "" {
		return Result{}, fmt.Errorf("%w: a funding names the account the money goes to", ErrInvalid)
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	offer, err := p.book.get("offer", req.OfferRef)
	if err != nil {
		return Result{}, err
	}
	if st := offer.status().Status; st != StatusCompleted {
		return Result{}, fmt.Errorf("%w: offer %s is %s and cannot be funded", ErrInvalid, req.OfferRef, st)
	}
	op, _, err := p.book.record("fund", req.ID, req, byAmount(offer.request.(CapitalRequest).Amount))
	if err != nil {
		return Result{}, err
	}
	return op.result, nil
}

// Repay takes nothing: it answers by the amount repaid.
func (p *TestCapitalPartner) Repay(_ context.Context, req Repayment) (Result, error) {
	if err := checkMoney(req.Amount); err != nil {
		return Result{}, err
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	funding, err := p.book.get("fund", req.FundingRef)
	if err != nil {
		return Result{}, err
	}
	offer, err := p.book.get("offer", funding.request.(Funding).OfferRef)
	if err != nil {
		return Result{}, err
	}
	if cur := offer.request.(CapitalRequest).Amount.Currency; cur != req.Amount.Currency {
		return Result{}, fmt.Errorf("%w: the funding is in %s, not %s", ErrInvalid, cur, req.Amount.Currency)
	}
	op, _, err := p.book.record("repay", req.ID, req, byAmount(req.Amount))
	if err != nil {
		return Result{}, err
	}
	return op.result, nil
}

// Status is where an offer, a funding or a repayment stands now.
func (p *TestCapitalPartner) Status(_ context.Context, ref string) (Result, error) {
	for _, kind := range []string{"offer", "fund", "repay"} {
		if strings.HasPrefix(ref, "test_"+kind+"_") {
			return p.book.status(kind, ref)
		}
	}
	return Result{}, fmt.Errorf("%w: %q", ErrNotFound, ref)
}
