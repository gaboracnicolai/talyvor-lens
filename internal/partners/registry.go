package partners

import (
	"context"
	"fmt"
	"sync"
)

// Service is one kind of outside service.
type Service string

// The services.
const (
	ServiceAccount    Service = "account"
	ServiceFX         Service = "fx"
	ServiceBroker     Service = "broker"
	ServiceStablecoin Service = "stablecoin"
	ServiceKYC        Service = "kyc"
	ServiceScreening  Service = "screening"
	ServiceCapital    Service = "capital"
	ServiceInsurer    Service = "insurer"
	ServiceAgentToken Service = "agent_token"
	ServiceTax        Service = "tax"
)

// Clearances says whether a wallet capability may take live money now: the operator's clearance for it is in
// force, and lists the country the use comes from (economy.DualTokenStore.CapabilityCleared).
type Clearances interface {
	CapabilityCleared(ctx context.Context, capability string) (bool, error)
}

// Registry hands out the partner for each service. Each is asked with the wallet capability the money moves
// for, and answers with the Test implementation unless that capability has a live clearance AND a real adapter
// is configured for the service. There is no real adapter yet.
//
// It keeps one Test implementation of each, so what one call does a later status or statement read sees.
type Registry struct {
	clearances Clearances

	mu       sync.RWMutex
	adapters map[Service]any

	account    *TestAccountPartner
	fx         *TestFXPartner
	broker     *TestBrokerPartner
	stablecoin *TestStablecoinPartner
	kyc        *TestKYCProvider
	capital    *TestCapitalPartner
	insurer    *TestInsurerPartner
	agentToken *TestAgentTokenProvider
	tax        *TestTaxPartner
}

// NewRegistry is a registry that asks clearances before it hands out a real adapter. With nil clearances it
// hands out the Test implementations only.
func NewRegistry(clearances Clearances) *Registry {
	return &Registry{clearances: clearances, adapters: map[Service]any{}, account: &TestAccountPartner{}, fx: &TestFXPartner{},
		broker: &TestBrokerPartner{}, stablecoin: &TestStablecoinPartner{}, kyc: &TestKYCProvider{}, capital: &TestCapitalPartner{},
		insurer: &TestInsurerPartner{}, agentToken: &TestAgentTokenProvider{}, tax: &TestTaxPartner{}}
}

// UseTaxData is where the Test tax partner reads the rates and registrations: a TaxStore.
func (r *Registry) UseTaxData(d TaxData) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tax = &TestTaxPartner{Data: d}
}

// Configure sets the real adapter for a service. It is used only once a capability the service serves is
// cleared; until then the Test implementation answers.
func (r *Registry) Configure(s Service, adapter any) error {
	ok := false
	switch s {
	case ServiceAccount:
		_, ok = adapter.(AccountPartner)
	case ServiceFX:
		_, ok = adapter.(FXPartner)
	case ServiceBroker:
		_, ok = adapter.(BrokerPartner)
	case ServiceStablecoin:
		_, ok = adapter.(StablecoinPartner)
	case ServiceKYC:
		_, ok = adapter.(KYCProvider)
	case ServiceScreening:
		_, ok = adapter.(ScreeningProvider)
	case ServiceCapital:
		_, ok = adapter.(CapitalPartner)
	case ServiceInsurer:
		_, ok = adapter.(InsurerPartner)
	case ServiceAgentToken:
		_, ok = adapter.(AgentTokenProvider)
	case ServiceTax:
		_, ok = adapter.(TaxPartner)
	default:
		return fmt.Errorf("partners: no service is called %q", s)
	}
	if !ok {
		return fmt.Errorf("partners: %T is not a %s partner", adapter, s)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[s] = adapter
	return nil
}

// pick is the configured adapter for s when capability is cleared, and test otherwise.
func pick[T any](ctx context.Context, r *Registry, s Service, capability string, test T) (T, error) {
	r.mu.RLock()
	adapter, configured := r.adapters[s]
	r.mu.RUnlock()
	if !configured || r.clearances == nil {
		return test, nil
	}
	cleared, err := r.clearances.CapabilityCleared(ctx, capability)
	if err != nil {
		var none T
		return none, fmt.Errorf("partners: is %s cleared: %w", capability, err)
	}
	if !cleared {
		return test, nil
	}
	return adapter.(T), nil
}

// Account is the account partner for capability.
func (r *Registry) Account(ctx context.Context, capability string) (AccountPartner, error) {
	return pick[AccountPartner](ctx, r, ServiceAccount, capability, r.account)
}

// FX is the conversion partner for capability.
func (r *Registry) FX(ctx context.Context, capability string) (FXPartner, error) {
	return pick[FXPartner](ctx, r, ServiceFX, capability, r.fx)
}

// Broker is the broker for capability.
func (r *Registry) Broker(ctx context.Context, capability string) (BrokerPartner, error) {
	return pick[BrokerPartner](ctx, r, ServiceBroker, capability, r.broker)
}

// Stablecoin is the stablecoin partner for capability.
func (r *Registry) Stablecoin(ctx context.Context, capability string) (StablecoinPartner, error) {
	return pick[StablecoinPartner](ctx, r, ServiceStablecoin, capability, r.stablecoin)
}

// KYC is the verification provider for capability.
func (r *Registry) KYC(ctx context.Context, capability string) (KYCProvider, error) {
	return pick[KYCProvider](ctx, r, ServiceKYC, capability, r.kyc)
}

// Verification is the provider the verification levels' checks go to (B30.4): the real one once it is configured,
// the Test one until then. A check moves no money, so no clearance is asked; a pass by the Test provider counts for
// test money only (economy.WorkspaceVerification).
func (r *Registry) Verification() KYCProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if adapter, ok := r.adapters[ServiceKYC]; ok {
		return adapter.(KYCProvider)
	}
	return r.kyc
}

// Screening is the screening provider for capability.
func (r *Registry) Screening(ctx context.Context, capability string) (ScreeningProvider, error) {
	return pick[ScreeningProvider](ctx, r, ServiceScreening, capability, TestScreeningProvider{})
}

// Capital is the capital partner for capability.
func (r *Registry) Capital(ctx context.Context, capability string) (CapitalPartner, error) {
	return pick[CapitalPartner](ctx, r, ServiceCapital, capability, r.capital)
}

// Insurer is the insurer for capability.
func (r *Registry) Insurer(ctx context.Context, capability string) (InsurerPartner, error) {
	return pick[InsurerPartner](ctx, r, ServiceInsurer, capability, r.insurer)
}

// AgentToken is the agent-token provider for capability.
func (r *Registry) AgentToken(ctx context.Context, capability string) (AgentTokenProvider, error) {
	return pick[AgentTokenProvider](ctx, r, ServiceAgentToken, capability, r.agentToken)
}

// Tax is the tax partner: the real one once it is configured (B32.45), and the Test one until then. Tax moves no
// money, so no clearance is asked.
func (r *Registry) Tax() TaxPartner {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if adapter, ok := r.adapters[ServiceTax]; ok {
		return adapter.(TaxPartner)
	}
	return r.tax
}
