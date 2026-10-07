package partners

import (
	"context"
	"testing"
)

// A real adapter for each service, as the registry would be configured with one: the Test implementation under
// another name.
type (
	realAccount    struct{ *TestAccountPartner }
	realFX         struct{ *TestFXPartner }
	realBroker     struct{ *TestBrokerPartner }
	realStablecoin struct{ *TestStablecoinPartner }
	realKYC        struct{ *TestKYCProvider }
	realScreening  struct{ TestScreeningProvider }
	realCapital    struct{ *TestCapitalPartner }
	realInsurer    struct{ *TestInsurerPartner }
	realAgentToken struct{ *TestAgentTokenProvider }
)

func (realAccount) Name() string    { return "real" }
func (realFX) Name() string         { return "real" }
func (realBroker) Name() string     { return "real" }
func (realStablecoin) Name() string { return "real" }
func (realKYC) Name() string        { return "real" }
func (realScreening) Name() string  { return "real" }
func (realCapital) Name() string    { return "real" }
func (realInsurer) Name() string    { return "real" }
func (realAgentToken) Name() string { return "real" }

// cleared is a Clearances with a fixed set of cleared capabilities.
type cleared map[string]bool

func (c cleared) CapabilityCleared(_ context.Context, capability string) (bool, error) {
	return c[capability], nil
}

type named interface{ Name() string }

func services(r *Registry) map[Service]struct {
	adapter any
	get     func(context.Context, string) (named, error)
} {
	type svc = struct {
		adapter any
		get     func(context.Context, string) (named, error)
	}
	return map[Service]svc{
		ServiceAccount: {realAccount{&TestAccountPartner{}}, func(ctx context.Context, c string) (named, error) { return r.Account(ctx, c) }},
		ServiceFX:      {realFX{&TestFXPartner{}}, func(ctx context.Context, c string) (named, error) { return r.FX(ctx, c) }},
		ServiceBroker:  {realBroker{&TestBrokerPartner{}}, func(ctx context.Context, c string) (named, error) { return r.Broker(ctx, c) }},
		ServiceStablecoin: {realStablecoin{&TestStablecoinPartner{}}, func(ctx context.Context, c string) (named, error) {
			return r.Stablecoin(ctx, c)
		}},
		ServiceKYC:       {realKYC{&TestKYCProvider{}}, func(ctx context.Context, c string) (named, error) { return r.KYC(ctx, c) }},
		ServiceScreening: {realScreening{}, func(ctx context.Context, c string) (named, error) { return r.Screening(ctx, c) }},
		ServiceCapital:   {realCapital{&TestCapitalPartner{}}, func(ctx context.Context, c string) (named, error) { return r.Capital(ctx, c) }},
		ServiceInsurer:   {realInsurer{&TestInsurerPartner{}}, func(ctx context.Context, c string) (named, error) { return r.Insurer(ctx, c) }},
		ServiceAgentToken: {realAgentToken{&TestAgentTokenProvider{}}, func(ctx context.Context, c string) (named, error) {
			return r.AgentToken(ctx, c)
		}},
	}
}

// The registry hands out the Test implementation of every service when the capability has no clearance, even with
// a real adapter configured; the adapter only once the capability is cleared; and the Test implementation for a
// cleared capability when no adapter is configured.
func TestRegistry_TestUnlessClearedAndConfigured(t *testing.T) {
	ctx := context.Background()
	const capability = "payments_out"
	check := func(r *Registry, s Service, get func(context.Context, string) (named, error), want string) {
		t.Helper()
		p, err := get(ctx, capability)
		if err != nil {
			t.Fatal(err)
		}
		if p.Name() != want {
			t.Fatalf("%s: the registry handed out %q, want %q", s, p.Name(), want)
		}
	}

	notCleared := NewRegistry(cleared{})
	for s, svc := range services(notCleared) {
		if err := notCleared.Configure(s, svc.adapter); err != nil {
			t.Fatal(err)
		}
		check(notCleared, s, svc.get, "test")
	}

	clearedNoAdapter := NewRegistry(cleared{capability: true})
	for s, svc := range services(clearedNoAdapter) {
		check(clearedNoAdapter, s, svc.get, "test")
	}

	clearedAndConfigured := NewRegistry(cleared{capability: true})
	for s, svc := range services(clearedAndConfigured) {
		if err := clearedAndConfigured.Configure(s, svc.adapter); err != nil {
			t.Fatal(err)
		}
		check(clearedAndConfigured, s, svc.get, "real")
	}

	if err := clearedAndConfigured.Configure(ServiceFX, realAccount{}); err == nil {
		t.Fatal("an account partner was configured as the FX partner")
	}
}
