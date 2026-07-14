package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/mining"
)

// trafficAuditRecorder is a fake trafficRevokeDB that tracks the record-before-
// revoke ORDERING: QueryRow (the audit INSERT) flips recorded=true; the fake
// revoker below reads that flag to prove the audit landed BEFORE the burn.
type trafficAuditRecorder struct {
	recorded  bool
	completed bool
}

type fakeIDRow struct{ id string }

func (r fakeIDRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if p, ok := dest[0].(*string); ok {
			*p = r.id
		}
	}
	return nil
}

func (d *trafficAuditRecorder) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	d.recorded = true
	return fakeIDRow{id: "aud-1"}
}

func (d *trafficAuditRecorder) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	d.completed = true
	return pgconn.CommandTag{}, nil
}

// orderCheckingRevoker records whether the audit was already written when the
// revoke fired (the record-before-revoke safety property) and the keys it got.
type orderCheckingRevoker struct {
	db             *trafficAuditRecorder
	sawRecordFirst bool
	gotKeys        []mining.TrafficHoldKey
}

func (r *orderCheckingRevoker) RevokeTrafficHolds(_ context.Context, keys []mining.TrafficHoldKey) (mining.TrafficRevokeReport, error) {
	r.sawRecordFirst = r.db.recorded
	r.gotKeys = keys
	out := map[string]mining.TrafficRevokeOutcome{}
	for _, k := range keys {
		out[k.RequestID+"|"+k.WorkspaceID+"|"+k.MintType] = mining.TrafficRevokeRevoked
	}
	return mining.TrafficRevokeReport{Outcomes: out, Totals: map[mining.TrafficRevokeOutcome]int{mining.TrafficRevokeRevoked: len(keys)}}, nil
}

func trafficBody() *strings.Reader {
	b, _ := json.Marshal(trafficRevokeRequest{
		ResolutionLabel: "manual",
		RevokeKeys:      []trafficRevokeKey{{RequestID: "r1", WorkspaceID: "wsN", MintType: "compute_mine"}},
	})
	return strings.NewReader(string(b))
}

// A non-admin is forbidden and NOTHING is recorded or revoked.
func TestTrafficRevokeHandler_NonAdminForbidden(t *testing.T) {
	db := &trafficAuditRecorder{}
	rev := &orderCheckingRevoker{db: db}
	h := newTrafficRevokeHandler(fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: false, UserID: "ws-7"}}, db, rev)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/x", trafficBody()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	if db.recorded || rev.sawRecordFirst || rev.gotKeys != nil {
		t.Fatalf("a forbidden request must not record or revoke (recorded=%v, revoked=%v)", db.recorded, rev.gotKeys != nil)
	}
}

// An admin request records the audit BEFORE the burn, revokes exactly the chosen
// composite keys, and completes the record.
func TestTrafficRevokeHandler_AdminRecordsBeforeRevoke(t *testing.T) {
	db := &trafficAuditRecorder{}
	rev := &orderCheckingRevoker{db: db}
	h := newTrafficRevokeHandler(fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: true, UserID: "admin-1"}}, db, rev)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/x", trafficBody()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if !rev.sawRecordFirst {
		t.Fatal("record-before-revoke violated: the audit row must be written BEFORE RevokeTrafficHolds fires")
	}
	if len(rev.gotKeys) != 1 || rev.gotKeys[0] != (mining.TrafficHoldKey{RequestID: "r1", WorkspaceID: "wsN", MintType: "compute_mine"}) {
		t.Fatalf("revoker got %+v, want the one composite key", rev.gotKeys)
	}
	if !db.completed {
		t.Fatal("the audit record must be completed with the outcome after revoke")
	}
}

// An empty operator subset is rejected (never auto-selects), and nothing is recorded.
func TestTrafficRevokeHandler_EmptyKeysRejected(t *testing.T) {
	db := &trafficAuditRecorder{}
	rev := &orderCheckingRevoker{db: db}
	h := newTrafficRevokeHandler(fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: true}}, db, rev)
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(trafficRevokeRequest{ResolutionLabel: "manual"})
	h(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(body))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (empty subset)", rec.Code)
	}
	if db.recorded {
		t.Fatal("an empty subset must not record an adjudication")
	}
}
