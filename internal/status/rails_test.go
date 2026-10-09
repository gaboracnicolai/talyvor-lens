package status

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
)

// fxCleared is a clearance reader where only fx has a clearance in force.
type fxCleared struct{}

func (fxCleared) WalletCapabilities(context.Context) ([]economy.CapabilityStatus, error) {
	out := []economy.CapabilityStatus{}
	for _, c := range economy.Capabilities {
		st := economy.CapabilityStatus{Capability: c}
		if c.Key == economy.CapabilityFX {
			st.Clearance = &economy.Clearance{}
		}
		out = append(out, st)
	}
	return out, nil
}

// B30.12: the page lists every partner in its mode with its clearances, a Test partner whose last call failed shows
// as down, one whose last call succeeded as up, and one never called as neither.
func TestStatus_MoneyRails(t *testing.T) {
	ctx := context.Background()
	reg := partners.NewRegistry(nil)
	scr, err := reg.Screening(ctx, economy.CapabilityPaymentsOut)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scr.ScreenName(ctx, partners.NameScreen{Name: "TESTFAIL Ltd"}); err == nil {
		t.Fatal("the Test screening provider screened a TESTFAIL name")
	}
	acct, err := reg.Account(ctx, economy.CapabilityCurrencyAccounts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acct.OpenAccount(ctx, partners.AccountRequest{ID: "a1", Holder: "Acme", Currency: "GBP"}); err != nil {
		t.Fatal(err)
	}

	page := newStatusPage(&fakePinger{}, nil, nil, "test")
	page.UseMoneyRails(reg, fxCleared{})
	page.UpdateCache(page.Check(ctx))

	rec := httptest.NewRecorder()
	page.ServeJSON(rec, httptest.NewRequest(http.MethodGet, "/status.json", nil))
	var got StatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rails) != len(partners.Services) {
		t.Fatalf("%d rails, want one per partner service (%d)", len(got.Rails), len(partners.Services))
	}
	rails := map[partners.Service]MoneyRail{}
	for _, r := range got.Rails {
		if r.Mode != "test" || r.Name == "" {
			t.Fatalf("%s: mode %q name %q, want a named Test partner", r.Service, r.Mode, r.Name)
		}
		rails[r.Service] = r
	}
	if s := rails[partners.ServiceScreening]; s.Status != StatusOutage || s.LastFailure == nil || s.LastSuccess != nil {
		t.Fatalf("the failing Test screening provider reads %s (success %v, failure %v), want an outage", s.Status, s.LastSuccess, s.LastFailure)
	}
	if a := rails[partners.ServiceAccount]; a.Status != StatusOperational || a.LastSuccess == nil {
		t.Fatalf("the account partner reads %s after a good call, want operational", a.Status)
	}
	if f := rails[partners.ServiceFX]; f.Status != StatusUnknown || len(f.Capabilities) != 1 || f.Capabilities[0].Cleared == nil || !*f.Capabilities[0].Cleared {
		t.Fatalf("fx reads %s with %+v, want never called and its capability cleared", f.Status, f.Capabilities)
	}

	html := renderHTML(&got)
	for _, want := range []string{"Money rails", "Preview — test money only",
		`Sanctions screening<div class="msg">Test partner · no clearance needed</div></td><td><span class="pill bad">outage</span>`,
		`Currency conversion<div class="msg">Test partner · cleared</div></td><td><span class="pill">no calls yet</span>`,
		`Accounts and payments<div class="msg">Test partner · not cleared</div></td><td><span class="pill good">operational</span>`} {
		if !strings.Contains(html, want) {
			t.Fatalf("the status page lacks %q", want)
		}
	}
	if strings.Contains(html, "TESTFAIL") {
		t.Fatal("the status page shows a partner's error text")
	}
}
