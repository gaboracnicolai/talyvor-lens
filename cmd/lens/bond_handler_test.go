package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/provenance"
)

type fakeBondCreator struct {
	bondID  string
	created bool
	err     error
	gotWS   string
	gotOID  string
	gotAmt  int64
	called  bool
}

func (f *fakeBondCreator) CreateBond(_ context.Context, ws, oid string, amt int64) (string, bool, error) {
	f.called = true
	f.gotWS, f.gotOID, f.gotAmt = ws, oid, amt
	return f.bondID, f.created, f.err
}

func TestBondCreate_Handler(t *testing.T) {
	serve := func(authn fakeAuthn, bc *fakeBondCreator, body string) *httptest.ResponseRecorder {
		r := chi.NewRouter()
		r.Post("/v1/bonds", newBondCreateHandler(authn, bc))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/bonds", strings.NewReader(body)))
		return w
	}
	body := `{"output_id":"oid-1","amount_ulens":1000000}`

	// authed + created → 200, scoped to the caller's workspace.
	bc := &fakeBondCreator{bondID: "b1", created: true}
	if w := serve(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-A"}}, bc, body); w.Code != http.StatusOK {
		t.Fatalf("created: status=%d, want 200", w.Code)
	}
	if bc.gotWS != "ws-A" || bc.gotOID != "oid-1" || bc.gotAmt != 1_000_000 {
		t.Errorf("bond must be scoped to caller+body; got ws=%q oid=%q amt=%d", bc.gotWS, bc.gotOID, bc.gotAmt)
	}

	// unauthenticated → 401, creator never reached (no collateral touched).
	un := &fakeBondCreator{}
	if w := serve(fakeAuthn{ctx: &auth.AuthContext{}}, un, body); w.Code != http.StatusUnauthorized {
		t.Errorf("unauth: status=%d, want 401", w.Code)
	}
	if un.called {
		t.Error("unauth must not reach the creator")
	}

	// bond on an unowned output → 403.
	no := &fakeBondCreator{err: provenance.ErrNotOwned}
	if w := serve(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-A"}}, no, body); w.Code != http.StatusForbidden {
		t.Errorf("not-owned: status=%d, want 403", w.Code)
	}

	// non-positive amount → 400, creator never reached.
	bad := &fakeBondCreator{}
	if w := serve(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-A"}}, bad, `{"output_id":"o","amount_ulens":0}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad amount: status=%d, want 400", w.Code)
	}
	if bad.called {
		t.Error("bad amount must not reach the creator")
	}
}

type fakeBondSettler struct {
	outcome string
	got     string
}

func (f *fakeBondSettler) SettleBond(_ context.Context, bondID string) (string, error) {
	f.got = bondID
	return f.outcome, nil
}

func TestBondSettle_Handler(t *testing.T) {
	r := chi.NewRouter()
	bs := &fakeBondSettler{outcome: "slashed"}
	r.Post("/v1/admin/bonds/{bond_id}/settle", newBondSettleHandler(bs))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/bonds/bond-xyz/settle", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("settle: status=%d, want 200", w.Code)
	}
	if bs.got != "bond-xyz" {
		t.Errorf("settle must use the path bond_id; got %q", bs.got)
	}
}
