package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/workspace"
)

// register_workspace_consent_test.go — declining cross-tenant pooling AT REGISTRATION.
//
// #357 made cache_poolable default TRUE for a NEW workspace. It is a SYMMETRIC consent flag: opting
// in to BENEFIT from the shared cache also DONATES this workspace's responses to other tenants. The
// registration body's `cache_poolable` was a plain Go bool, so an omitted field and an explicit
// `false` were indistinguishable — both decoded to false and were then overwritten by the
// new-workspace default. A privacy-conscious tenant therefore could NOT decline at creation; it had
// to discover the default afterwards and call SetCachePoolable(false).
//
// These tests pin the wire contract at the level that matters — the STORED row, not the response:
//
//	absent          -> true   (the #357 default still applies)
//	explicit false  -> false  (HONOURED — this is the behaviour being added)
//	explicit true   -> true   (honoured)
//	existing row    -> PRESERVED, whatever the body says (the #357 boundary, unchanged)
const (
	wsConsentAbsent    = "ws-consent-absent"
	wsConsentFalse     = "ws-consent-explicit-false"
	wsConsentTrue      = "ws-consent-explicit-true"
	wsConsentExisting  = "ws-consent-existing-optout"
	wsConsentExistingT = "ws-consent-existing-optout-forced"
)

// storedPoolable reads the PERSISTED cache_poolable for a workspace. The tests assert on this — not
// on the handler's response body — because the row is what governs whether this tenant's responses
// are served to other tenants.
func storedPoolable(t *testing.T, pool *pgxpool.Pool, wsID string) bool {
	t.Helper()
	var poolable bool
	if err := pool.QueryRow(context.Background(),
		`SELECT cache_poolable FROM workspaces WHERE id=$1`, wsID).Scan(&poolable); err != nil {
		t.Fatalf("read stored cache_poolable for %q: %v", wsID, err)
	}
	return poolable
}

func freshWorkspace(t *testing.T, pool *pgxpool.Pool, wsID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, wsID); err != nil {
		t.Fatalf("clean %q: %v", wsID, err)
	}
}

// A NEW workspace that says nothing about pooling gets the #357 default: poolable.
func TestRegister_PoolingFieldAbsent_StoresDefaultTrue(t *testing.T) {
	reg, pool := wsRegisterManager(t)
	freshWorkspace(t, pool, wsConsentAbsent)

	w := serveRegister(newRegisterWorkspaceHandler(reg), `{"id":"`+wsConsentAbsent+`","name":"a"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want 201", w.Code, w.Body.String())
	}
	if got := storedPoolable(t, pool, wsConsentAbsent); !got {
		t.Errorf("stored cache_poolable=%v, want true (an absent field keeps the new-workspace default)", got)
	}
}

// A NEW workspace that explicitly declines is STORED as declined. This is the behaviour #357 could
// not express: before this change the explicit false decoded to Go's zero value, was
// indistinguishable from "absent", and was overwritten by the default.
func TestRegister_PoolingExplicitFalse_IsHonoured(t *testing.T) {
	reg, pool := wsRegisterManager(t)
	freshWorkspace(t, pool, wsConsentFalse)

	w := serveRegister(newRegisterWorkspaceHandler(reg),
		`{"id":"`+wsConsentFalse+`","name":"a","cache_poolable":false}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want 201", w.Code, w.Body.String())
	}
	if got := storedPoolable(t, pool, wsConsentFalse); got {
		t.Errorf("stored cache_poolable=%v, want false — an explicit decline at registration must be HONOURED, not overwritten by the default", got)
	}
}

// An explicit true is honoured too (it agrees with the default, but it must not be treated as "absent").
func TestRegister_PoolingExplicitTrue_IsHonoured(t *testing.T) {
	reg, pool := wsRegisterManager(t)
	freshWorkspace(t, pool, wsConsentTrue)

	w := serveRegister(newRegisterWorkspaceHandler(reg),
		`{"id":"`+wsConsentTrue+`","name":"a","cache_poolable":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want 201", w.Code, w.Body.String())
	}
	if got := storedPoolable(t, pool, wsConsentTrue); !got {
		t.Errorf("stored cache_poolable=%v, want true", got)
	}
}

// ⚠ #357's BOUNDARY, unchanged: an EXISTING opted-out workspace re-registered with the field ABSENT
// stays false. The new-workspace default must never be applied retroactively.
func TestRegister_ExistingOptOut_FieldAbsent_StaysFalse(t *testing.T) {
	reg, pool := wsRegisterManager(t)
	freshWorkspace(t, pool, wsConsentExisting)

	if err := reg.RegisterWorkspace(context.Background(),
		workspace.Workspace{ID: wsConsentExisting, Name: "e"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := reg.SetCachePoolable(context.Background(), wsConsentExisting, false); err != nil {
		t.Fatalf("opt out: %v", err)
	}

	w := serveRegister(newRegisterWorkspaceHandler(reg), `{"id":"`+wsConsentExisting+`","name":"e2"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want 201", w.Code, w.Body.String())
	}
	if got := storedPoolable(t, pool, wsConsentExisting); got {
		t.Errorf("stored cache_poolable=%v, want false — an existing opt-out must survive re-registration", got)
	}
}

// ⚠ THE SHARPEST RISK THIS CHANGE INTRODUCES: now that an explicit value can reach the manager, an
// explicit `true` in a re-registration body must STILL not flip an existing opted-out workspace.
// Honouring an explicit choice applies to CREATION only; consent is never re-granted retroactively.
func TestRegister_ExistingOptOut_ExplicitTrue_StaysFalse(t *testing.T) {
	reg, pool := wsRegisterManager(t)
	freshWorkspace(t, pool, wsConsentExistingT)

	if err := reg.RegisterWorkspace(context.Background(),
		workspace.Workspace{ID: wsConsentExistingT, Name: "e"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := reg.SetCachePoolable(context.Background(), wsConsentExistingT, false); err != nil {
		t.Fatalf("opt out: %v", err)
	}

	w := serveRegister(newRegisterWorkspaceHandler(reg),
		`{"id":"`+wsConsentExistingT+`","name":"e2","cache_poolable":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want 201", w.Code, w.Body.String())
	}
	if got := storedPoolable(t, pool, wsConsentExistingT); got {
		t.Errorf("stored cache_poolable=%v, want false — an explicit true must NOT re-pool an existing opted-out workspace", got)
	}
}

// The rule is symmetric and has no exceptions: registration never changes an EXISTING workspace's
// consent in EITHER direction, so an explicit false is refused on an existing poolable workspace too.
//
// This is deliberate, not an oversight. POST /v1/workspaces is an upsert used by provisioning
// automation; if a decline were honoured here, a static template carrying cache_poolable:false would
// silently revoke consent for every workspace it re-provisions — the same silent-revocation bug the
// sibling distill_poolable flag still has (it IS listed in the ON CONFLICT clause). A rule with an
// exception is where consent bugs live. SetCachePoolable is the purpose-built path for changing an
// existing workspace, and the response below makes the refusal VISIBLE rather than silent.
func TestRegister_ExistingPoolable_ExplicitFalse_IsRefusedAndVisible(t *testing.T) {
	reg, pool := wsRegisterManager(t)
	const wsID = "ws-consent-existing-poolable"
	freshWorkspace(t, pool, wsID)

	// An existing workspace that IS poolable (the new-workspace default).
	if err := reg.RegisterWorkspace(context.Background(),
		workspace.Workspace{ID: wsID, Name: "p"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := storedPoolable(t, pool, wsID); !got {
		t.Fatalf("seed precondition: stored=%v, want true", got)
	}

	w := serveRegister(newRegisterWorkspaceHandler(reg),
		`{"id":"`+wsID+`","name":"p2","cache_poolable":false}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want 201", w.Code, w.Body.String())
	}
	if got := storedPoolable(t, pool, wsID); !got {
		t.Errorf("stored cache_poolable=%v, want true — registration must not change an EXISTING workspace's consent in either direction (use SetCachePoolable)", got)
	}
	// And the caller is TOLD the decline did not take effect.
	var out struct {
		CachePoolable *bool `json:"cache_poolable"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.CachePoolable == nil || !*out.CachePoolable {
		t.Errorf("response cache_poolable=%v, want true — a refused decline must be visible, not silent", out.CachePoolable)
	}
}

// The registration response states the pooling state the workspace actually ended up in, so a client
// can RENDER it instead of guessing. It must report what was STORED — including when the caller's
// explicit true was refused because the workspace already existed and had opted out.
func TestRegister_ResponseReportsEffectivePoolingState(t *testing.T) {
	reg, pool := wsRegisterManager(t)

	decode := func(t *testing.T, body []byte) (id string, poolable *bool) {
		t.Helper()
		var out struct {
			ID            string `json:"id"`
			CachePoolable *bool  `json:"cache_poolable"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode response %s: %v", body, err)
		}
		return out.ID, out.CachePoolable
	}

	// (a) a new workspace that declined
	freshWorkspace(t, pool, wsConsentFalse)
	w := serveRegister(newRegisterWorkspaceHandler(reg),
		`{"id":"`+wsConsentFalse+`","name":"a","cache_poolable":false}`)
	id, got := decode(t, w.Body.Bytes())
	if id != wsConsentFalse {
		t.Errorf("id=%q, want %q", id, wsConsentFalse)
	}
	if got == nil {
		t.Fatal("response omits cache_poolable — a caller cannot render the pooling state it cannot see")
	}
	if *got {
		t.Errorf("response cache_poolable=%v, want false", *got)
	}

	// (b) an existing opt-out whose explicit true was refused: the response must say false (what was
	// stored), never echo the request.
	freshWorkspace(t, pool, wsConsentExistingT)
	if err := reg.RegisterWorkspace(context.Background(),
		workspace.Workspace{ID: wsConsentExistingT, Name: "e"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := reg.SetCachePoolable(context.Background(), wsConsentExistingT, false); err != nil {
		t.Fatalf("opt out: %v", err)
	}
	w = serveRegister(newRegisterWorkspaceHandler(reg),
		`{"id":"`+wsConsentExistingT+`","name":"e2","cache_poolable":true}`)
	_, got = decode(t, w.Body.Bytes())
	if got == nil {
		t.Fatal("response omits cache_poolable")
	}
	if *got {
		t.Errorf("response cache_poolable=%v, want false — the response must report the STORED state, not echo the request", *got)
	}
}
