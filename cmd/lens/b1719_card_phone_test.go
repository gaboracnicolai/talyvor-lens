package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v81"

	"github.com/talyvor/lens/internal/agentcard"
)

// B17.19 — Stripe issues no card to a cardholder without a phone number ("You cannot create a card without a
// `phone_number` on file"), so every test card was refused. The cardholder now has one: the person's own, or
// the unallocated test number when they give none, and the card is issued.
func TestB1719_ACardIsIssuedWithThePhoneNumberStripeNeeds(t *testing.T) {
	var phones []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/issuing/cardholders":
			phones = append(phones, form.Get("phone_number"))
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "ich_b1719", "object": "issuing.cardholder"})
		case "/v1/issuing/cards":
			if form.Get("cardholder") != "ich_b1719" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "invalid_request_error", "message": "no such cardholder"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "ic_b1719", "object": "issuing.card", "last4": "4242",
				"exp_month": 10, "exp_year": 2029, "currency": "gbp", "livemode": false})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	prev := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0),
	}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prev) })

	const path = "/v1/workspaces/ws-b2612/agents/agt-1/card"
	for _, c := range []struct{ body, phone string }{
		{`{"first_name":"Test","last_name":"Shopper","line1":"1 High Street","city":"London","postal_code":"EC1A 1BB"}`, "+447700900000"},
		{`{"first_name":"Test","last_name":"Shopper","phone_number":"+447700900123","line1":"1 High Street","city":"London","postal_code":"EC1A 1BB"}`, "+447700900123"},
	} {
		phones = nil
		r := chi.NewRouter()
		mountAgentCardRoutes(r, b2612Bank{}, agentcard.NewStripe("sk_test_b1719", "gbp"))
		if code, why := b2612Post(t, r, path, c.body); code != http.StatusCreated {
			t.Fatalf("issuing a test card = %d %q, want 201", code, why)
		}
		if len(phones) != 1 || phones[0] != c.phone {
			t.Fatalf("the cardholder went to Stripe with phone_number %q, want %q", phones, c.phone)
		}
	}
}
