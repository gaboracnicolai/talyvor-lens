package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// dashboard_routes.go — the browser-surface gate.
//
// dashReg mirrors billReg (and econReg before it) exactly: when the flag is off
// the routes are NEVER REGISTERED, so chi answers its own generic 404 — wire-
// identical to a path that was never built. That is the established shape in this
// binary for "this deployment does not have that surface", and a second shape
// would be a second thing to reason about. It is gated on cfg.DashboardEnabled,
// independent of both the U3 economy master and the U18b billing switch: whether
// Lens serves a page to a browser has nothing to do with either.
//
// DEFAULT OFF. Lens is an API. An operator who wants it to serve a browser page
// at /dashboard says so; the root page below is what a visitor gets regardless.
type dashReg struct{ on bool }

func (d dashReg) get(r chi.Router, pattern string, h http.HandlerFunc) {
	if d.on {
		r.Get(pattern, h)
	}
}
