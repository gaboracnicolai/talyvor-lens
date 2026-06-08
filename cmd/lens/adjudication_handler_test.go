package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/poolroyalty"
)

// fakeAuthenticator returns a fixed AuthContext (admin or not) — mirrors how the
// ApproveRate handler authenticates via authManager.Authenticate(req).
type fakeAuthenticator struct {
	ctx *auth.AuthContext
	err error
}

func (f fakeAuthenticator) Authenticate(_ *http.Request) (*auth.AuthContext, error) {
	return f.ctx, f.err
}

// fakeAdjudicator records whether Adjudicate fired and with what decision — so
// the auth test can assert ZERO record/revoke on the 403 path.
type fakeAdjudicator struct {
	called   bool
	decision poolroyalty.AdjudicationDecision
}

func (f *fakeAdjudicator) Adjudicate(_ context.Context, d poolroyalty.AdjudicationDecision) (string, poolroyalty.RevokeReport, error) {
	f.called = true
	f.decision = d
	return "adj-1", poolroyalty.RevokeReport{
		Outcomes: map[string]poolroyalty.RevokeOutcome{"req-1": poolroyalty.OutcomeRevoked},
		Totals:   map[poolroyalty.RevokeOutcome]int{poolroyalty.OutcomeRevoked: 1},
	}, nil
}

func adjBody() *bytes.Buffer {
	b, _ := json.Marshal(map[string]any{
		"flag_type":             "volume",
		"resolution_label":      "tuple_pinned",
		"candidate_request_ids": []string{"req-1", "req-2"},
		"revoke_request_ids":    []string{"req-1"},
	})
	return bytes.NewBuffer(b)
}

// AUTH GATE — non-admin → 403, and the adjudicator is NEVER called (zero
// record, zero revoke). Mirrors ApproveRate's IsAdmin gate.
func TestAdjudicateHandler_NonAdminForbidden(t *testing.T) {
	adj := &fakeAdjudicator{}
	h := newAdjudicateHandler(fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: false, UserID: "ws-7"}}, adj)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/v1/admin/pool-royalty/adjudicate", adjBody()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if adj.called {
		t.Fatal("a non-admin must NOT reach the adjudicator (zero record, zero revoke)")
	}
}

// AUTH error from the manager → 403, no adjudication.
func TestAdjudicateHandler_AuthErrorForbidden(t *testing.T) {
	adj := &fakeAdjudicator{}
	h := newAdjudicateHandler(fakeAuthenticator{err: http.ErrNoCookie}, adj)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/x", adjBody()))
	if rec.Code != http.StatusForbidden || adj.called {
		t.Fatalf("auth error must 403 with no adjudication; code=%d called=%v", rec.Code, adj.called)
	}
}

// ADMIN → proceeds: the adjudicator is called with the decoded decision, the
// operator-chosen subset is passed through, decided_by defaults to global_key
// when UserID is empty, and the response carries the id + report.
func TestAdjudicateHandler_AdminProceeds(t *testing.T) {
	adj := &fakeAdjudicator{}
	h := newAdjudicateHandler(fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: true, UserID: ""}}, adj)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/x", adjBody()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !adj.called {
		t.Fatal("admin path must call the adjudicator")
	}
	if len(adj.decision.RevokeRequestIDs) != 1 || adj.decision.RevokeRequestIDs[0] != "req-1" {
		t.Errorf("chosen subset not passed through: %v", adj.decision.RevokeRequestIDs)
	}
	if adj.decision.DecidedBy != "global_key" {
		t.Errorf("decided_by = %q, want global_key (empty UserID → global_key, the ApproveRate precedent)", adj.decision.DecidedBy)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["adjudication_id"] != "adj-1" {
		t.Errorf("response missing adjudication_id; got %v", resp)
	}
}

// Admin with a real UserID → decided_by records it.
func TestAdjudicateHandler_AdminUserIDRecorded(t *testing.T) {
	adj := &fakeAdjudicator{}
	h := newAdjudicateHandler(fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: true, UserID: "admin-jane"}}, adj)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/x", adjBody()))
	if adj.decision.DecidedBy != "admin-jane" {
		t.Errorf("decided_by = %q, want admin-jane", adj.decision.DecidedBy)
	}
}

// Empty revoke set → 400 (the handler rejects before any side effect path that matters).
func TestAdjudicateHandler_EmptyRevokeSet(t *testing.T) {
	adj := &fakeAdjudicator{}
	h := newAdjudicateHandler(fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: true}}, adj)
	body, _ := json.Marshal(map[string]any{"flag_type": "volume", "resolution_label": "tuple_pinned", "revoke_request_ids": []string{}})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewBuffer(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty revoke set → status %d, want 400", rec.Code)
	}
}
