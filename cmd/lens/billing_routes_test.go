package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
)

// fakePurchaseLister returns canned rows (incl. an anomalous one) so the admin
// purchases ROUTE — billReg gating + requireAdmin — is provable over HTTP without
// a database. The real ListPurchases SQL is covered in internal/billing.
type fakePurchaseLister struct {
	rows   []billing.Purchase
	err    error
	called bool
}

func (f *fakePurchaseLister) ListPurchases(context.Context, int) ([]billing.Purchase, error) {
	f.called = true
	return f.rows, f.err
}

// TestBillingPurchases_AdminGate — GET /v1/admin/billing/purchases, mounted exactly
// as run() does (billReg.get + requireAdmin + newBillingPurchasesHandler):
//
//	(a) admin + billing ON  → 200, rows visible incl. the 'anomalous' row;
//	(b) billing OFF         → 404 (unregistered), handler never reached;
//	(c) non-admin           → 401, handler never reached.
func TestBillingPurchases_AdminGate(t *testing.T) {
	credited := billing.Purchase{StripeEventID: "evt_c", WorkspaceID: "wsA", USDCents: 1000, LXCAmount: 100, Status: "completed"}
	anomalous := billing.Purchase{StripeEventID: "evt_a", WorkspaceID: "wsA", USDCents: 1000, LXCAmount: 0, Status: "anomalous"}

	serve := func(on bool, authn fakeAuthn, lister *fakePurchaseLister) *httptest.ResponseRecorder {
		r := chi.NewRouter()
		billReg{on: on}.get(r, "/v1/admin/billing/purchases",
			requireAdmin(authn, newBillingPurchasesHandler(lister)))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/billing/purchases", nil))
		return rec
	}

	// (a) admin + billing on → 200, both rows visible incl. anomalous.
	lister := &fakePurchaseLister{rows: []billing.Purchase{credited, anomalous}}
	rec := serve(true, fakeAuthn{ctx: &auth.AuthContext{IsAdmin: true}}, lister)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin+on: status=%d, want 200", rec.Code)
	}
	if !lister.called {
		t.Error("admin+on: the lister must be reached")
	}
	if !strings.Contains(rec.Body.String(), `"status":"anomalous"`) {
		t.Errorf("admin+on: the anomalous row must be visible; body=%s", rec.Body.String())
	}

	// (b) billing disabled → 404 (unregistered), handler never reached.
	off := &fakePurchaseLister{rows: []billing.Purchase{credited}}
	rec = serve(false, fakeAuthn{ctx: &auth.AuthContext{IsAdmin: true}}, off)
	if rec.Code != http.StatusNotFound {
		t.Errorf("billing off: status=%d, want 404 (unregistered)", rec.Code)
	}
	if off.called {
		t.Error("billing off: the lister must NOT be reached")
	}

	// (c) non-admin → 401, handler never reached (fail closed).
	nonAdmin := &fakePurchaseLister{rows: []billing.Purchase{credited}}
	rec = serve(true, fakeAuthn{ctx: &auth.AuthContext{IsAdmin: false, WorkspaceID: "wsA"}}, nonAdmin)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("non-admin: status=%d, want 401", rec.Code)
	}
	if nonAdmin.called {
		t.Error("non-admin: the lister must NOT be reached (refused before the handler)")
	}
}
