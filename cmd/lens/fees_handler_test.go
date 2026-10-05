package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// B32.8 — GET /v1/public/fees answers every fee Nicolai decided, with his values.
func TestPublicFees_EveryFeeWithNicolaisValues(t *testing.T) {
	w := httptest.NewRecorder()
	publicFeesHandler(w, httptest.NewRequest(http.MethodGet, "/v1/public/fees", nil))
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); w.Code != http.StatusOK || err != nil {
		t.Fatalf("GET /v1/public/fees = %d %s (%v)", w.Code, w.Body, err)
	}
	want := map[string]any{
		"market_take_bps":        1500.0,
		"services_take_bps":      500.0,
		"compute_take_bps":       500.0,
		"lending_fee_bps":        100.0,
		"platform_fee_bps":       map[string]any{"free": 550.0, "team": 300.0, "business": 100.0, "enterprise": 100.0},
		"fx_margin_bps":          map[string]any{"free": 0.0, "team": 50.0, "business": 25.0, "enterprise": 15.0},
		"intl_payment_fee_minor": map[string]any{"GBP": 500.0, "EUR": 600.0, "USD": 700.0},
		"merchant_fee_bps":       75.0,
		"merchant_a2a_fee_bps":   100.0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GET /v1/public/fees = %v\nwant %v", got, want)
	}
}
