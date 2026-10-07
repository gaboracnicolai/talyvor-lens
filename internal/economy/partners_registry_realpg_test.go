package economy

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/partners"
)

// realAccountPartner stands for a real adapter: the Test partner under another name.
type realAccountPartner struct{ *partners.TestAccountPartner }

func (realAccountPartner) Name() string { return "real" }

// B30.3 — the partners registry asks the operator's clearances (wallet_clearances): with a real adapter configured
// it hands out the Test partner until the capability is cleared, the adapter while the clearance is in force and
// lists the country the use comes from, and the Test partner again once the clearance is revoked.
func TestPartnersRegistry_AsksTheCapabilitysClearance(t *testing.T) {
	pool := supplyPool(t)
	s := NewDualTokenStore(nil, pool, nil)
	gb, fr := WithUseCountry(context.Background(), "GB"), WithUseCountry(context.Background(), "FR")
	r := partners.NewRegistry(s)
	if err := r.Configure(partners.ServiceAccount, realAccountPartner{&partners.TestAccountPartner{}}); err != nil {
		t.Fatal(err)
	}
	partner := func(ctx context.Context) string {
		t.Helper()
		p, err := r.Account(ctx, CapabilityPaymentsOut)
		if err != nil {
			t.Fatal(err)
		}
		return p.Name()
	}

	if got := partner(gb); got != "test" {
		t.Fatalf("with no clearance the registry handed out %q", got)
	}
	if _, err := s.ClearCapability(gb, CapabilityPaymentsOut, "nicolai", ClearanceTerms{Reference: "B30.3 test", Licence: "EMI-900123",
		Partner: "Test Payments Ltd", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got := partner(gb); got != "real" {
		t.Fatalf("cleared for GB, a use from GB got %q", got)
	}
	if got := partner(fr); got != "test" {
		t.Fatalf("cleared for GB only, a use from FR got %q", got)
	}
	if err := s.RevokeClearance(gb, CapabilityPaymentsOut, "nicolai", "B30.3 test over"); err != nil {
		t.Fatal(err)
	}
	if got := partner(gb); got != "test" {
		t.Fatalf("after the revoke the registry handed out %q", got)
	}
}
