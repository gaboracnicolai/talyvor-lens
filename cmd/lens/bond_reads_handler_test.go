package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/provenance"
)

type fakeBondReader struct {
	list  []provenance.Bond
	byID  map[string]provenance.Bond // key "ws|bondID"
	gotWS string
}

func (f *fakeBondReader) ListByWorkspace(_ context.Context, ws string, _ int) ([]provenance.Bond, error) {
	f.gotWS = ws
	return f.list, nil
}
func (f *fakeBondReader) GetByID(_ context.Context, ws, bondID string) (provenance.Bond, bool, error) {
	b, ok := f.byID[ws+"|"+bondID]
	return b, ok, nil
}

// GET /v1/bonds/{bond_id}: owner-scoped. Own bond → 200 with appeal_deadline surfaced. A bond the caller
// doesn't own → 404 (NOT 403, no oracle). No credential → 401.
func TestBondGet_Handler(t *testing.T) {
	deadline := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	reader := &fakeBondReader{byID: map[string]provenance.Bond{
		"wsA|bond-1": {BondID: "bond-1", WorkspaceID: "wsA", OutputID: "oid", Status: "active", AmountULens: 1000000, AppealDeadline: deadline},
	}}
	serve := func(ac *auth.AuthContext, bondID string) *httptest.ResponseRecorder {
		r := chi.NewRouter()
		r.Get("/v1/bonds/{bond_id}", newBondGetHandler(fakeAuthn{ctx: ac}, reader))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bonds/"+bondID, nil))
		return rec
	}
	// own bond → 200 + appeal_deadline present (a UI needs it)
	rec := serve(&auth.AuthContext{WorkspaceID: "wsA"}, "bond-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("own bond: status=%d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "appeal_deadline") {
		t.Errorf("appeal_deadline must be surfaced; body=%s", rec.Body.String())
	}
	// foreign / unknown bond → 404, NOT 403
	rec = serve(&auth.AuthContext{WorkspaceID: "wsA"}, "bond-foreign")
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign bond: status=%d, want 404 (not 403 — no cross-tenant oracle)", rec.Code)
	}
	// no credential → 401
	rec = serve(&auth.AuthContext{}, "bond-1")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no credential: status=%d, want 401", rec.Code)
	}
}

// GET /v1/bonds: self-scoped array of the caller's own bonds.
func TestBondList_Handler(t *testing.T) {
	reader := &fakeBondReader{list: []provenance.Bond{{BondID: "b1", WorkspaceID: "wsA"}}}
	h := newBondListHandler(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "wsA"}}, reader)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/bonds", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if reader.gotWS != "wsA" {
		t.Errorf("must scope to caller's workspace; got %q", reader.gotWS)
	}
	if !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") {
		t.Errorf("bond list must be a JSON array; body=%s", rec.Body.String())
	}
}

// Flag gate (behavioral): with H5 bonds OFF the read routes are NOT registered (chi 404) — matching their
// POST/settle write siblings; ON → present.
func TestBondReadRoutes_FlagGatedAbsent(t *testing.T) {
	reader := &fakeBondReader{list: []provenance.Bond{}}
	authn := fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "wsA"}}
	hit := func(enabled bool) int {
		r := chi.NewRouter()
		registerBondReadRoutes(r, enabled, authn, reader)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bonds", nil))
		return rec.Code
	}
	if code := hit(false); code != http.StatusNotFound {
		t.Errorf("H5 bonds OFF: GET /v1/bonds must be 404 (route absent), got %d", code)
	}
	if code := hit(true); code != http.StatusOK {
		t.Errorf("H5 bonds ON: GET /v1/bonds must be 200, got %d", code)
	}
}

// The production wiring gates the read routes on cfg.H5BondsEnabled (the same flag as the write siblings).
func TestBondReadRoutes_WiredToFlag(t *testing.T) {
	src := mainGoSource(t)
	if !strings.Contains(src, "registerBondReadRoutes(authed, cfg.H5BondsEnabled") {
		t.Fatal("bond read routes must be registered via registerBondReadRoutes(authed, cfg.H5BondsEnabled, ...)")
	}
}
