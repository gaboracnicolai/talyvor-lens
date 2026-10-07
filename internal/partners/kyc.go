package partners

import (
	"context"
	"fmt"
	"strings"
)

// KYCProvider checks who a person or a company is, for the verification levels (B30.4): a person's identity, a
// company's number, directors and people with significant control. Lens keeps the provider's reference, never
// a document.
type KYCProvider interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// StartCheck starts a check. Idempotent on req.ID.
	StartCheck(ctx context.Context, req KYCRequest) (Result, error)
	// CheckResult says where a check stands now: completed is passed, failed is not, returned is a pass the
	// provider has since withdrawn.
	CheckResult(ctx context.Context, ref string) (Result, error)
}

// The subjects a check is about.
const (
	KYCPerson  = "person"
	KYCCompany = "company"
)

// KYCRequest asks for a check on a person or a company.
type KYCRequest struct {
	ID            string `json:"id"` // the caller's id for the check
	Subject       string `json:"subject"`
	Name          string `json:"name"`
	Country       string `json:"country"`                  // ISO 3166-1 alpha-2
	DateOfBirth   string `json:"date_of_birth,omitempty"`  // a person's, YYYY-MM-DD
	CompanyNumber string `json:"company_number,omitempty"` // a company's
}

// TestKYCProvider is test mode for verification: it checks nobody. A check answers by the name (see the package
// comment): TESTFAIL and TESTSANCTION fail, TESTRETURN passes and is later withdrawn, TESTPENDING stays pending,
// anything else passes.
type TestKYCProvider struct {
	book testBook
}

// Name is "test".
func (*TestKYCProvider) Name() string { return "test" }

// StartCheck checks nobody: it answers by the name.
func (p *TestKYCProvider) StartCheck(_ context.Context, req KYCRequest) (Result, error) {
	switch {
	case req.Subject != KYCPerson && req.Subject != KYCCompany:
		return Result{}, fmt.Errorf("%w: a check is on a person or a company, not %q", ErrInvalid, req.Subject)
	case strings.TrimSpace(req.Name) == "":
		return Result{}, fmt.Errorf("%w: a check names who it is on", ErrInvalid)
	case req.Subject == KYCCompany && strings.TrimSpace(req.CompanyNumber) == "":
		return Result{}, fmt.Errorf("%w: a company check carries the company number", ErrInvalid)
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	op, _, err := p.book.record("kyc", req.ID, req, byName(req.Name))
	if err != nil {
		return Result{}, err
	}
	return op.result, nil
}

// CheckResult is where a check stands now.
func (p *TestKYCProvider) CheckResult(_ context.Context, ref string) (Result, error) {
	return p.book.status("kyc", ref)
}
