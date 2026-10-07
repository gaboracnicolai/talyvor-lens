package partners

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ScreeningProvider screens names and payments against sanctions lists before any money moves (B30.6). A
// screening is an answer now: it has nothing to come back.
type ScreeningProvider interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// ScreenName screens one name.
	ScreenName(ctx context.Context, req NameScreen) (Screening, error)
	// ScreenPayment screens a payment's payer and payee.
	ScreenPayment(ctx context.Context, req PaymentScreen) (Screening, error)
}

// The outcomes of a screening.
const (
	ScreenClear  = "clear"  // no match: the money may move
	ScreenHit    = "hit"    // a match: the money must not move
	ScreenReview = "review" // a close match: the money waits for a person to decide
)

// ErrScreeningUnavailable: the provider could not screen. Nothing may move on it.
var ErrScreeningUnavailable = errors.New("partners: screening is unavailable")

// NameScreen is a name to screen.
type NameScreen struct {
	Name        string `json:"name"`
	Country     string `json:"country,omitempty"`
	DateOfBirth string `json:"date_of_birth,omitempty"`
}

// PaymentScreen is a payment to screen.
type PaymentScreen struct {
	ID     string `json:"id"` // the caller's id for the payment
	Payer  string `json:"payer"`
	Payee  string `json:"payee"`
	Amount Money  `json:"amount"`
}

// Screening is a screening's outcome, and what matched.
type Screening struct {
	Outcome string           `json:"outcome"`
	Matches []ScreeningMatch `json:"matches"`
	Detail  string           `json:"detail,omitempty"`
}

// ScreeningMatch is one list entry a name matched, and how closely, in basis points (10,000 is exact).
type ScreeningMatch struct {
	Name     string `json:"name"`
	List     string `json:"list"`
	Entry    string `json:"entry"`
	ScoreBPS int    `json:"score_bps"`
}

// TestScreeningProvider is test mode for screening: it reads no list. A name containing TESTSANCTION is a hit,
// TESTPENDING is held for review, TESTFAIL makes the provider unavailable, and anything else is clear.
type TestScreeningProvider struct{}

// Name is "test".
func (TestScreeningProvider) Name() string { return "test" }

// ScreenName screens the name against the test words.
func (TestScreeningProvider) ScreenName(_ context.Context, req NameScreen) (Screening, error) {
	return testScreen(req.Name)
}

// ScreenPayment screens the payer and the payee: the worse outcome of the two.
func (TestScreeningProvider) ScreenPayment(_ context.Context, req PaymentScreen) (Screening, error) {
	if err := checkID(req.ID); err != nil {
		return Screening{}, err
	}
	if err := checkMoney(req.Amount); err != nil {
		return Screening{}, err
	}
	out := Screening{Outcome: ScreenClear, Matches: []ScreeningMatch{}}
	for _, name := range []string{req.Payer, req.Payee} {
		s, err := testScreen(name)
		if err != nil {
			return Screening{}, err
		}
		out.Matches = append(out.Matches, s.Matches...)
		if s.Outcome == ScreenHit || (s.Outcome == ScreenReview && out.Outcome == ScreenClear) {
			out.Outcome, out.Detail = s.Outcome, s.Detail
		}
	}
	return out, nil
}

func testScreen(name string) (Screening, error) {
	if strings.TrimSpace(name) == "" {
		return Screening{}, fmt.Errorf("%w: a screening names who it screens", ErrInvalid)
	}
	n := strings.ToUpper(name)
	match := func(score int) []ScreeningMatch {
		return []ScreeningMatch{{Name: name, List: "TEST", Entry: "TESTSANCTION", ScoreBPS: score}}
	}
	switch {
	case strings.Contains(n, "TESTFAIL"):
		return Screening{}, fmt.Errorf("%w: test mode: names containing TESTFAIL make screening unavailable", ErrScreeningUnavailable)
	case strings.Contains(n, "TESTSANCTION"):
		return Screening{Outcome: ScreenHit, Matches: match(10_000), Detail: "test mode: names containing TESTSANCTION are on a sanctions list"}, nil
	case strings.Contains(n, "TESTPENDING"):
		return Screening{Outcome: ScreenReview, Matches: match(9_000), Detail: "test mode: names containing TESTPENDING are held for review"}, nil
	}
	return Screening{Outcome: ScreenClear, Matches: []ScreeningMatch{}}, nil
}
