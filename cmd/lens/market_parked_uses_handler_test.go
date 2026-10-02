package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/market"
)

type parkedUsesFake []market.ParkedUse

func (f parkedUsesFake) ParkedUses(context.Context) ([]market.ParkedUse, error) { return f, nil }

// B26.3 — the operator's read names each parked use with Stripe's reason.
func TestMarketParkedUses_NamesEachWithStripesReason(t *testing.T) {
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	h := newMarketParkedUsesHandler(parkedUsesFake{{ID: "use_1", ListingID: "lst_1", BuyerWorkspaceID: "ws-buyer", PriceULXC: 500_000,
		UsedAt: at, Refusals: market.MaxMeterRefusals, Reason: "resource_missing: No such customer: 'cus_gone'", ParkedAt: at}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/marketplace/parked-uses", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var body struct {
		ParkedUses []struct {
			ID               string `json:"id"`
			BuyerWorkspaceID string `json:"buyer_workspace_id"`
			Refusals         int    `json:"refusals"`
			Reason           string `json:"reason"`
		} `json:"parked_uses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.ParkedUses) != 1 || body.ParkedUses[0].ID != "use_1" || body.ParkedUses[0].BuyerWorkspaceID != "ws-buyer" ||
		body.ParkedUses[0].Refusals != market.MaxMeterRefusals || body.ParkedUses[0].Reason != "resource_missing: No such customer: 'cus_gone'" {
		t.Fatalf("parked uses = %s; want use_1 with its refusals and Stripe's reason", w.Body)
	}
}
