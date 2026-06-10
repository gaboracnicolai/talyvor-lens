package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/distillattrib"
)

type fakeDistillAttribReader struct {
	gotLimit              int
	rawCalled, pairCalled bool
}

func (f *fakeDistillAttribReader) RawRows(_ context.Context, limit int) ([]distillattrib.ServeRow, error) {
	f.gotLimit, f.rawCalled = limit, true
	return []distillattrib.ServeRow{{OwnerWorkspaceID: "wsA", RequesterWorkspaceID: "wsB", ContentHash: "h1", ServeCount: 7}}, nil
}

func (f *fakeDistillAttribReader) PairTotals(_ context.Context, limit int) ([]distillattrib.PairTotal, error) {
	f.gotLimit, f.pairCalled = limit, true
	return []distillattrib.PairTotal{{OwnerWorkspaceID: "wsA", RequesterWorkspaceID: "wsB", Serves: 42}}, nil
}

func serveDistillAdmin(target string) (*httptest.ResponseRecorder, *fakeDistillAttribReader) {
	f := &fakeDistillAttribReader{}
	rec := httptest.NewRecorder()
	newDistillAttributionAdminHandler(f)(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec, f
}

func TestDistillAttributionAdmin_DefaultRawRows(t *testing.T) {
	rec, f := serveDistillAdmin("/v1/admin/distill/attribution")
	if rec.Code != http.StatusOK || !f.rawCalled || f.pairCalled {
		t.Fatalf("default: code=%d raw=%v pair=%v, want 200/raw-only", rec.Code, f.rawCalled, f.pairCalled)
	}
	if f.gotLimit != distillAttribLimitDefault {
		t.Errorf("default limit = %d, want %d", f.gotLimit, distillAttribLimitDefault)
	}
}

func TestDistillAttributionAdmin_PairsView(t *testing.T) {
	rec, f := serveDistillAdmin("/v1/admin/distill/attribution?view=pairs")
	if rec.Code != http.StatusOK || !f.pairCalled || f.rawCalled {
		t.Fatalf("pairs: code=%d raw=%v pair=%v, want 200/pairs-only", rec.Code, f.rawCalled, f.pairCalled)
	}
}

func TestDistillAttributionAdmin_LimitCapped(t *testing.T) {
	// Over the cap → ignored (stays default); within → honored. The cap can't be
	// bypassed to dump the whole table unbounded.
	if _, fOver := serveDistillAdmin("/v1/admin/distill/attribution?limit=999999"); fOver.gotLimit != distillAttribLimitDefault {
		t.Errorf("over-cap limit = %d, want default %d (cap not bypassable)", fOver.gotLimit, distillAttribLimitDefault)
	}
	if _, fOk := serveDistillAdmin("/v1/admin/distill/attribution?limit=250"); fOk.gotLimit != 250 {
		t.Errorf("within-cap limit = %d, want 250", fOk.gotLimit)
	}
}

// The admin gate (requireAdmin): a non-admin is refused 401 and the reader is
// NEVER reached — content_hash + counterparty workspace ids never exposed.
func TestDistillAttributionAdmin_RequireAdminGate(t *testing.T) {
	tenant := fakeAuthn{ctx: &auth.AuthContext{IsAdmin: false, WorkspaceID: "wsA"}}

	f := &fakeDistillAttribReader{}
	gated := requireAdmin(tenant, http.HandlerFunc(newDistillAttributionAdminHandler(f)))
	rec := httptest.NewRecorder()
	gated(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/distill/attribution", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("non-admin: code = %d, want 401", rec.Code)
	}
	if f.rawCalled || f.pairCalled {
		t.Error("LEAK: non-admin reached the distill attribution reader (content_hash + wsIDs exposed)")
	}

	// Admin passes; the raw view carries content_hash (admin-only exposure).
	fa := &fakeDistillAttribReader{}
	adminGated := requireAdmin(fakeAuthn{ctx: &auth.AuthContext{IsAdmin: true}}, http.HandlerFunc(newDistillAttributionAdminHandler(fa)))
	recA := httptest.NewRecorder()
	adminGated(recA, httptest.NewRequest(http.MethodGet, "/v1/admin/distill/attribution", nil))
	if recA.Code != http.StatusOK || !fa.rawCalled {
		t.Fatalf("admin: code=%d raw=%v, want 200/reached", recA.Code, fa.rawCalled)
	}
	if !strings.Contains(recA.Body.String(), "content_hash") {
		t.Error("admin raw response should carry content_hash")
	}
}
