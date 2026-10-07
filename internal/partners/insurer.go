package partners

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// InsurerPartner covers an owner against their agents' mistakes: the capability cover.
type InsurerPartner interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// Quote prices cover. Idempotent on req.ID.
	Quote(ctx context.Context, req CoverRequest) (CoverQuote, error)
	// Bind takes cover out at a quote. Idempotent on req.ID.
	Bind(ctx context.Context, req BindRequest) (Policy, error)
	// Claim claims on a policy. Idempotent on req.ID.
	Claim(ctx context.Context, req ClaimRequest) (Result, error)
	// ClaimStatus says where a claim stands now: completed is paid, failed declined, returned paid and clawed back.
	ClaimStatus(ctx context.Context, ref string) (Result, error)
}

// CoverRequest asks the price of cover.
type CoverRequest struct {
	ID       string `json:"id"` // the caller's id for the quote
	Insured  string `json:"insured"`
	Cover    Money  `json:"cover"` // the most a claim pays
	TermDays int    `json:"term_days"`
	Risk     string `json:"risk"` // what is covered, in words
}

// CoverQuote is the price of cover.
type CoverQuote struct {
	Ref     string `json:"partner_ref"`
	Cover   Money  `json:"cover"`
	Premium Money  `json:"premium"`
}

// BindRequest takes cover out at a quote.
type BindRequest struct {
	ID       string `json:"id"` // the caller's id for the policy
	QuoteRef string `json:"quote_ref"`
}

// Policy is cover in force from From until To.
type Policy struct {
	Result
	Cover Money     `json:"cover"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
}

// ClaimRequest claims on a policy.
type ClaimRequest struct {
	ID        string `json:"id"` // the caller's id for the claim
	PolicyRef string `json:"policy_ref"`
	Amount    Money  `json:"amount"`
	What      string `json:"what"` // what went wrong, in words
}

// TestInsurerPartner is test mode for cover: it covers nothing and charges nothing — a premium is the insurer's
// price, and there is no insurer. Every quote binds; a claim over the cover is declined, and any other answers by
// its amount (see the package comment).
type TestInsurerPartner struct {
	book testBook
}

// Name is "test".
func (*TestInsurerPartner) Name() string { return "test" }

// Quote quotes the cover asked for, at no premium.
func (p *TestInsurerPartner) Quote(_ context.Context, req CoverRequest) (CoverQuote, error) {
	if err := checkMoney(req.Cover); err != nil {
		return CoverQuote{}, err
	}
	if strings.TrimSpace(req.Insured) == "" || req.TermDays <= 0 {
		return CoverQuote{}, fmt.Errorf("%w: cover names who it insures and runs at least a day", ErrInvalid)
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	op, _, err := p.book.record("coverq", req.ID, req, func(string) (Result, Status) {
		return Result{Status: StatusCompleted}, StatusCompleted
	})
	if err != nil {
		return CoverQuote{}, err
	}
	return CoverQuote{Ref: op.result.Ref, Cover: req.Cover, Premium: Money{0, req.Cover.Currency}}, nil
}

// Bind takes the quoted cover out from now, for the quote's term.
func (p *TestInsurerPartner) Bind(_ context.Context, req BindRequest) (Policy, error) {
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	q, err := p.book.get("coverq", req.QuoteRef)
	if err != nil {
		return Policy{}, err
	}
	op, _, err := p.book.record("policy", req.ID, req, func(string) (Result, Status) {
		return Result{Status: StatusCompleted}, StatusCompleted
	})
	if err != nil {
		return Policy{}, err
	}
	cover := q.request.(CoverRequest)
	return Policy{Result: op.result, Cover: cover.Cover, From: op.at, To: op.at.AddDate(0, 0, cover.TermDays)}, nil
}

// Claim pays nothing: a claim over the cover, or outside the policy's term, is declined; any other answers by its
// amount.
func (p *TestInsurerPartner) Claim(_ context.Context, req ClaimRequest) (Result, error) {
	if err := checkMoney(req.Amount); err != nil {
		return Result{}, err
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	policy, err := p.book.get("policy", req.PolicyRef)
	if err != nil {
		return Result{}, err
	}
	q, err := p.book.get("coverq", policy.request.(BindRequest).QuoteRef)
	if err != nil {
		return Result{}, err
	}
	cover := q.request.(CoverRequest)
	decide := byAmount(req.Amount)
	now := p.book.clock()
	switch {
	case req.Amount.Currency != cover.Cover.Currency || req.Amount.Minor > cover.Cover.Minor:
		decide = declined("test mode: the claim is more than the cover")
	case now.After(policy.at.AddDate(0, 0, cover.TermDays)):
		decide = declined("test mode: the policy has ended")
	}
	op, _, err := p.book.record("claim", req.ID, req, decide)
	if err != nil {
		return Result{}, err
	}
	return op.result, nil
}

func declined(why string) func(string) (Result, Status) {
	return func(string) (Result, Status) { return Result{Status: StatusFailed, Detail: why}, StatusFailed }
}

// ClaimStatus is where a claim stands now.
func (p *TestInsurerPartner) ClaimStatus(_ context.Context, ref string) (Result, error) {
	return p.book.status("claim", ref)
}
