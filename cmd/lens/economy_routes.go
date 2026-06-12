package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// economy_routes.go — U3 master economy kill-switch, the ROUTE chokepoint.
//
// econReg is the single gate through which EVERY economy route is registered.
// When the master switch is off (cfg.EconomyEnabled == false), the route is
// never registered, so chi serves its native 404 — indistinguishable from a
// path that never existed (#152, no existence oracle). The config-side gate
// force-off (config.Load) stops state creation; this stops the surface.
//
// Adding a new economy route? Register it through econ.{get,post,del} (NOT the
// bare router) — cmd/lens/economy_killswitch_test.go walks main.go and FAILS if
// an economy-manifest route is registered without this guard.
type econReg struct{ on bool }

func (e econReg) get(r chi.Router, pattern string, h http.HandlerFunc) {
	if e.on {
		r.Get(pattern, h)
	}
}

func (e econReg) post(r chi.Router, pattern string, h http.HandlerFunc) {
	if e.on {
		r.Post(pattern, h)
	}
}

func (e econReg) del(r chi.Router, pattern string, h http.HandlerFunc) {
	if e.on {
		r.Delete(pattern, h)
	}
}
