package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

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

type retryFake map[string]bool

func (f retryFake) RetryParkedUse(_ context.Context, id string) error {
	if !f[id] {
		return market.ErrNotParked
	}
	delete(f, id)
	return nil
}

// B27.19 — the operator's retry un-parks the use it names, and says so; one not parked is a 404.
func TestMarketParkedUseRetry_UnparksTheUseItNames(t *testing.T) {
	r := chi.NewRouter()
	parked := retryFake{"use_1": true}
	r.Post("/v1/admin/marketplace/parked-uses/{useID}/retry", newMarketParkedUseRetryHandler(parked).ServeHTTP)
	for _, tc := range []struct {
		id   string
		code int
	}{{"use_1", http.StatusOK}, {"use_1", http.StatusNotFound}, {"use_2", http.StatusNotFound}} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/marketplace/parked-uses/"+tc.id+"/retry", nil))
		if w.Code != tc.code {
			t.Fatalf("retry %s = %d %s; want %d", tc.id, w.Code, w.Body, tc.code)
		}
	}
	if parked["use_1"] {
		t.Fatal("use_1 is still parked after its retry")
	}
}
