package partners

import (
	"context"
	"errors"
	"testing"
	"time"
)

// B37.2 — every rail is probed with a read-only question.

type noTaxRows struct{}

func (noTaxRows) TaxRate(context.Context, string, string, time.Time) (TaxRate, bool, error) {
	return TaxRate{}, false, nil
}
func (noTaxRows) TaxRegistration(context.Context, string, time.Time) (TaxRegistration, bool, error) {
	return TaxRegistration{}, false, nil
}

// Every Test partner answers its probe, a reference it does not know as not found, and WatchRails shows each rail
// answering as soon as it starts.
func TestProbe_EveryTestRailAnswersAtStart(t *testing.T) {
	r := NewRegistry(nil)
	r.UseTaxData(noTaxRows{})
	for s, err := range r.probe(context.Background()) {
		if err != nil && !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: the probe was not answered: %v", s, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.WatchRails(ctx, nil, time.Hour)
	deadline := time.Now().Add(5 * time.Second)
	for _, rail := range r.Rails() {
		for rail.LastSuccess == nil && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			rail = r.Rails()[indexOf(rail.Service)]
		}
		if rail.LastSuccess == nil || rail.LastFailure != nil || rail.Mode != "test" {
			t.Errorf("%s: want a test rail with a last success and no failure, got %+v", rail.Service, rail)
		}
	}
}

func indexOf(s Service) int {
	for i, x := range Services {
		if x == s {
			return i
		}
	}
	return -1
}

// Each fake partner implements only its probe's read-only method; any other method, money-moving or not, is the nil
// interface it embeds and panics. Each answers an error while down is set.
type probeFakes struct{ down bool }

func (f *probeFakes) err() error {
	if f.down {
		return errors.New("partner unreachable")
	}
	return nil
}

type (
	fakeAccount struct {
		AccountPartner
		f *probeFakes
	}
	fakeFX struct {
		FXPartner
		f *probeFakes
	}
	fakeBroker struct {
		BrokerPartner
		f *probeFakes
	}
	fakeStablecoin struct {
		StablecoinPartner
		f *probeFakes
	}
	fakeKYC struct {
		KYCProvider
		f *probeFakes
	}
	fakeScreening struct {
		ScreeningProvider
		f *probeFakes
	}
	fakeCapital struct {
		CapitalPartner
		f *probeFakes
	}
	fakeInsurer struct {
		InsurerPartner
		f *probeFakes
	}
	fakeAgentToken struct {
		AgentTokenProvider
		f *probeFakes
	}
	fakeTax struct {
		TaxPartner
		f *probeFakes
	}
)

func (p fakeAccount) AccountDetails(context.Context, string) (AccountDetails, error) {
	return AccountDetails{}, p.f.err()
}
func (p fakeFX) Quote(context.Context, FXQuoteRequest) (FXQuote, error) { return FXQuote{}, p.f.err() }
func (p fakeBroker) Quote(context.Context, string, string) (BrokerQuote, error) {
	return BrokerQuote{}, p.f.err()
}
func (p fakeStablecoin) Status(context.Context, string) (Result, error) { return Result{}, p.f.err() }
func (p fakeKYC) CheckResult(context.Context, string) (Result, error)   { return Result{}, p.f.err() }
func (p fakeScreening) ScreenName(context.Context, NameScreen) (Screening, error) {
	return Screening{}, p.f.err()
}
func (p fakeCapital) Offer(context.Context, CapitalRequest) (CapitalOffer, error) {
	return CapitalOffer{}, p.f.err()
}
func (p fakeInsurer) Quote(context.Context, CoverRequest) (CoverQuote, error) {
	return CoverQuote{}, p.f.err()
}
func (p fakeAgentToken) Status(context.Context, string) (Result, error) { return Result{}, p.f.err() }
func (p fakeTax) Calculate(context.Context, TaxRequest) (TaxResult, error) {
	return TaxResult{}, p.f.err()
}

// No probe calls a money-moving method of a live adapter, and a partner that fails reads down until it answers again.
func TestProbe_CallsNoMoneyMovingMethod_AndADownRailComesBack(t *testing.T) {
	f := &probeFakes{down: true}
	r := NewRegistry(nil)
	for s, a := range map[Service]any{ServiceAccount: fakeAccount{f: f}, ServiceFX: fakeFX{f: f}, ServiceBroker: fakeBroker{f: f},
		ServiceStablecoin: fakeStablecoin{f: f}, ServiceKYC: fakeKYC{f: f}, ServiceScreening: fakeScreening{f: f},
		ServiceCapital: fakeCapital{f: f}, ServiceInsurer: fakeInsurer{f: f}, ServiceAgentToken: fakeAgentToken{f: f},
		ServiceTax: fakeTax{f: f}} {
		if err := r.Configure(s, a); err != nil {
			t.Fatal(err)
		}
	}
	probe := func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("a probe called a method other than its read-only one: %v", p)
			}
		}()
		r.Probe(context.Background())
	}

	probe()
	for _, rail := range r.Rails() {
		if rail.Mode != "live" || rail.LastFailure == nil || rail.LastSuccess != nil {
			t.Errorf("%s: a failing partner reads down, got %+v", rail.Service, rail)
		}
	}
	f.down = false
	probe()
	for _, rail := range r.Rails() {
		if rail.LastSuccess == nil || rail.LastFailure.After(*rail.LastSuccess) {
			t.Errorf("%s: a partner that answers again reads up, got %+v", rail.Service, rail)
		}
	}
}
