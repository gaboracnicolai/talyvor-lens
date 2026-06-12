package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// billing_routes.go — U18b FIAT billing surface gate.
//
// billReg mirrors econReg but is gated on cfg.BillingEnabled, INDEPENDENT of the
// U3 economy master switch: billing is fiat (Stripe → LXC credit), so it must be
// registrable with the economy OFF (a pure fiat-SaaS deployment runs
// EconomyEnabled=false + BillingEnabled=true). When billing is off the routes are
// never registered → chi-native 404. Billing routes are NOT economy: they must
// NOT be registered through econReg, and must stay OUT of the economy manifest.
type billReg struct{ on bool }

func (b billReg) get(r chi.Router, pattern string, h http.HandlerFunc) {
	if b.on {
		r.Get(pattern, h)
	}
}

func (b billReg) post(r chi.Router, pattern string, h http.HandlerFunc) {
	if b.on {
		r.Post(pattern, h)
	}
}
