package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
)

type clearancesFake struct{ cleared economy.ClearanceTerms }

func (f *clearancesFake) WalletCapabilities(context.Context) ([]economy.CapabilityStatus, error) {
	var out []economy.CapabilityStatus
	for _, c := range economy.Capabilities {
		st := economy.CapabilityStatus{Capability: c, RealMoney: c.Class == economy.ClassGreen}
		if c.Key == economy.CapabilityFX && f.cleared.Licence != "" {
			st.RealMoney, st.Clearance = true, &economy.Clearance{By: "nicolai", At: time.Now(), ClearanceTerms: f.cleared}
		}
		out = append(out, st)
	}
	return out, nil
}
func (f *clearancesFake) ClearCapability(_ context.Context, _, _ string, terms economy.ClearanceTerms) (economy.Clearance, error) {
	f.cleared = terms
	return economy.Clearance{By: "nicolai", At: time.Now(), ClearanceTerms: terms}, nil
}
func (f *clearancesFake) RevokeClearance(context.Context, string, string, string) error { return nil }
func (f *clearancesFake) ClearanceLog(context.Context, int) ([]economy.ClearanceRecord, error) {
	return nil, nil
}

// B30.1 — `lens clearances clear` records the licence, partner, countries and expiry, and `lens clearances` then
// lists every capability with its class, and the clearance's terms beside the one it clears.
func TestClearancesCommand_ClearsWithTermsAndListsThem(t *testing.T) {
	ctx := context.Background()
	store := &clearancesFake{}
	var out bytes.Buffer
	if err := walletClearancesCommand(ctx, store, []string{"clear", "fx", "--licence", "EMI-900123", "--partner", "Test FX Ltd",
		"--countries", "GB,IE", "--expires", "2027-10-01", "partner", "agreement", "PA-9"}, "nicolai", &out); err != nil {
		t.Fatal(err)
	}
	want := economy.ClearanceTerms{Reference: "partner agreement PA-9", Licence: "EMI-900123", Partner: "Test FX Ltd",
		Countries: []string{"GB", "IE"}, ExpiresAt: time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC)}
	if got := store.cleared; got.Reference != want.Reference || got.Licence != want.Licence || got.Partner != want.Partner ||
		strings.Join(got.Countries, ",") != "GB,IE" || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("cleared with %+v, want %+v", got, want)
	}
	if err := walletClearancesCommand(ctx, store, []string{"clear", "fx", "partner agreement PA-9"}, "nicolai", &out); err == nil {
		t.Fatal("a clear naming no licence, partner, countries or expiry was accepted")
	}

	out.Reset()
	if err := walletClearancesCommand(ctx, store, nil, "nicolai", &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(economy.Capabilities) {
		t.Fatalf("listed %d capabilities, want %d", len(lines), len(economy.Capabilities))
	}
	listed := out.String()
	for _, row := range []string{
		"RED\tfx\tConverting between currencies\treal money from GB,IE until 2027-10-01T00:00:00Z: licence EMI-900123, partner Test FX Ltd",
		"AMBER\tb2b_credit\tCredit lines and loans to companies\ttest money only",
		"RED\tpayouts_to_people\tPaying people for tasks\ttest money only",
	} {
		if !strings.Contains(listed, row) {
			t.Errorf("the list is missing %q:\n%s", row, listed)
		}
	}
}
