package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/lens/internal/batch"
	"github.com/talyvor/lens/internal/eval"
	"github.com/talyvor/lens/internal/povi"
	"github.com/talyvor/lens/internal/session"
)

// authz_routes_phase3_test.go — behavioral guards for the #146 Phase-3
// IDOR/node-keyed sub-class: tenant A acting on tenant B's object by id must be
// refused, AND the dependency confirms no action (key NOT revoked). Reads 404 on
// cross-tenant (no oracle vs genuine not-found); writes refuse.

// ── api-key revoke (the revoke-any-key DoS) ──

type fakeKeyStore struct {
	owners  map[string]string
	revoked map[string]bool
}

func (f *fakeKeyStore) WorkspaceForKeyID(_ context.Context, id string) (string, bool, error) {
	ws, ok := f.owners[id]
	return ws, ok, nil
}
func (f *fakeKeyStore) Revoke(_ context.Context, id string) error {
	if f.revoked == nil {
		f.revoked = map[string]bool{}
	}
	f.revoked[id] = true
	return nil
}

func TestAuthzP3_RevokeAPIKey_DoSGuard(t *testing.T) {
	ks := &fakeKeyStore{owners: map[string]string{"keyB": "ws-B", "keyA": "ws-A"}, revoked: map[string]bool{}}
	h := newRevokeAPIKeyHandler(ks)

	// ATTACK: ws-A revokes ws-B's key → key must NOT be revoked (still valid).
	serveAuthed(t, http.MethodDelete, "/v1/api/keys/{keyID}", "/v1/api/keys/keyB", "", "ws-A", false, h)
	if ks.revoked["keyB"] {
		t.Fatal("DoS: ws-A revoked ws-B's key — B's key must remain valid")
	}
	// OWNER: ws-B revokes its own key → revoked.
	serveAuthed(t, http.MethodDelete, "/v1/api/keys/{keyID}", "/v1/api/keys/keyB", "", "ws-B", false, h)
	if !ks.revoked["keyB"] {
		t.Fatal("owner must be able to revoke its own key")
	}
	// ADMIN: revokes any key.
	serveAuthed(t, http.MethodDelete, "/v1/api/keys/{keyID}", "/v1/api/keys/keyA", "", "ws-adm", true, h)
	if !ks.revoked["keyA"] {
		t.Fatal("admin must be able to revoke any key")
	}
	// NONEXISTENT: silent no-op 200 (anti-oracle, unchanged behavior).
	rec := serveAuthed(t, http.MethodDelete, "/v1/api/keys/{keyID}", "/v1/api/keys/ghost", "", "ws-A", false, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("nonexistent key: got %d, want 200 (no oracle)", rec.Code)
	}
}

// ── IDOR reads: 404 on cross-tenant, identical to genuine not-found ──

type fakeSessionGetter struct {
	ws     string
	exists bool
}

func (f *fakeSessionGetter) GetSession(id string) (*session.Session, bool) {
	if !f.exists {
		return nil, false
	}
	return &session.Session{ID: id, WorkspaceID: f.ws}, true
}

func TestAuthzP3_SessionGet_IDOR(t *testing.T) {
	h := newSessionGetHandler(&fakeSessionGetter{ws: "ws-B", exists: true})
	// ws-A fetching ws-B's session → 404 (no body leak, no existence oracle).
	if rec := serveAuthed(t, http.MethodGet, "/v1/sessions/{sessionID}", "/v1/sessions/s1", "", "ws-A", false, h); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant session: got %d, want 404", rec.Code)
	}
	// owner → 200.
	if rec := serveAuthed(t, http.MethodGet, "/v1/sessions/{sessionID}", "/v1/sessions/s1", "", "ws-B", false, h); rec.Code != http.StatusOK {
		t.Fatalf("owner session: got %d, want 200", rec.Code)
	}
	// admin → 200.
	if rec := serveAuthed(t, http.MethodGet, "/v1/sessions/{sessionID}", "/v1/sessions/s1", "", "ws-adm", true, h); rec.Code != http.StatusOK {
		t.Fatalf("admin session: got %d, want 200", rec.Code)
	}
	// genuinely missing → 404 (same as cross-tenant: no oracle).
	hMiss := newSessionGetHandler(&fakeSessionGetter{exists: false})
	if rec := serveAuthed(t, http.MethodGet, "/v1/sessions/{sessionID}", "/v1/sessions/s1", "", "ws-A", false, hMiss); rec.Code != http.StatusNotFound {
		t.Fatalf("missing session: got %d, want 404", rec.Code)
	}
}

type fakeEvalRunGetter struct {
	ws     string
	exists bool
}

func (f *fakeEvalRunGetter) GetRun(_ context.Context, id string) (*eval.RunSummary, error) {
	if !f.exists {
		return nil, errors.New("run not found")
	}
	return &eval.RunSummary{RunID: id, WorkspaceID: f.ws}, nil
}

func TestAuthzP3_EvalRunGet_IDOR(t *testing.T) {
	h := newEvalRunGetHandler(&fakeEvalRunGetter{ws: "ws-B", exists: true})
	if rec := serveAuthed(t, http.MethodGet, "/v1/eval/runs/{runID}", "/v1/eval/runs/r1", "", "ws-A", false, h); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant run: got %d, want 404", rec.Code)
	}
	if rec := serveAuthed(t, http.MethodGet, "/v1/eval/runs/{runID}", "/v1/eval/runs/r1", "", "ws-B", false, h); rec.Code != http.StatusOK {
		t.Fatalf("owner run: got %d, want 200", rec.Code)
	}
	if rec := serveAuthed(t, http.MethodGet, "/v1/eval/runs/{runID}", "/v1/eval/runs/r1", "", "ws-adm", true, h); rec.Code != http.StatusOK {
		t.Fatalf("admin run: got %d, want 200", rec.Code)
	}
}

type fakeChallengeGetter struct {
	ws     string
	exists bool
}

func (f *fakeChallengeGetter) Get(_ context.Context, _ string) (*povi.Challenge, error) {
	if !f.exists {
		return nil, errors.New("not found")
	}
	return &povi.Challenge{WorkspaceID: f.ws}, nil
}

func TestAuthzP3_ChallengeGet_IDOR(t *testing.T) {
	h := newChallengeGetHandler(&fakeChallengeGetter{ws: "ws-B", exists: true})
	if rec := serveAuthed(t, http.MethodGet, "/v1/povi/challenges/{id}", "/v1/povi/challenges/c1", "", "ws-A", false, h); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant challenge: got %d, want 404", rec.Code)
	}
	if rec := serveAuthed(t, http.MethodGet, "/v1/povi/challenges/{id}", "/v1/povi/challenges/c1", "", "ws-B", false, h); rec.Code != http.StatusOK {
		t.Fatalf("owner challenge: got %d, want 200", rec.Code)
	}
}

type fakeBatchJobGetter struct {
	ws     string
	exists bool
}

func (f *fakeBatchJobGetter) GetJobByRequestID(id string) *batch.BatchJob {
	if !f.exists {
		return nil
	}
	return &batch.BatchJob{RequestID: id, WorkspaceID: f.ws}
}

func TestAuthzP3_BatchStatus_IDOR(t *testing.T) {
	h := newBatchStatusHandler(&fakeBatchJobGetter{ws: "ws-B", exists: true})
	if rec := serveAuthed(t, http.MethodGet, "/v1/batch/status/{requestID}", "/v1/batch/status/b1", "", "ws-A", false, h); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant batch job: got %d, want 404 (leaks prompt+response otherwise)", rec.Code)
	}
	if rec := serveAuthed(t, http.MethodGet, "/v1/batch/status/{requestID}", "/v1/batch/status/b1", "", "ws-B", false, h); rec.Code != http.StatusOK {
		t.Fatalf("owner batch job: got %d, want 200", rec.Code)
	}
}

// ── node-keyed ops: resolve node→workspace, 403 on cross-tenant ──

type fakeNodeResolver struct {
	ws  string
	err error
}

func (f *fakeNodeResolver) WorkspaceForNode(_ context.Context, _ string) (string, error) {
	return f.ws, f.err
}

func TestAuthzP3_RequireNodeOwnership(t *testing.T) {
	reqAs := func(ws string, admin bool) *http.Request {
		return withIdentity(httptest.NewRequest(http.MethodPost, "/", nil), ws, admin)
	}
	// cross-tenant → refused (403).
	rec := httptest.NewRecorder()
	if requireNodeOwnership(rec, reqAs("ws-A", false), &fakeNodeResolver{ws: "ws-B"}, "node1") || rec.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant node: passed=%v code=%d, want refused+403", rec.Code == http.StatusForbidden, rec.Code)
	}
	// owner → pass.
	rec = httptest.NewRecorder()
	if !requireNodeOwnership(rec, reqAs("ws-A", false), &fakeNodeResolver{ws: "ws-A"}, "node1") {
		t.Fatal("owner node op must pass")
	}
	// admin → pass.
	rec = httptest.NewRecorder()
	if !requireNodeOwnership(rec, reqAs("ws-adm", true), &fakeNodeResolver{ws: "ws-B"}, "node1") {
		t.Fatal("admin node op must pass")
	}
	// unknown node (resolver error) → refused with the unchanged 400 surface.
	rec = httptest.NewRecorder()
	if requireNodeOwnership(rec, reqAs("ws-A", false), &fakeNodeResolver{err: errors.New("node not found")}, "node1") || rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown node: code=%d, want refused+400", rec.Code)
	}
}
