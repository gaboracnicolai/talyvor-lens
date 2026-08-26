package main

// session_key_handler.go — W4.6.1 STEP 4: the HTTP boundary for session-scoped keys.
//
// ⚠ THIS ROUTE IS A CREDENTIAL ASKING FOR A CREDENTIAL, which is the exact shape that went wrong
// next door. docs/model2-session-credentials.md measures it: POST /v1/workspaces/{wsID}/api-keys
// has no scope gate and takes the new key's scopes from the request body, so any credential owning
// the workspace can mint one carrying scopes it does not itself hold. Three properties keep this
// route from repeating that, and each has a named test in session_key_handler_test.go:
//
//   WHO      exactly one credential shape may mint — a browser SESSION JWT. Everything else is
//            refused, INCLUDING a session key, so the credential cannot renew itself into an
//            unbounded chain and make its own TTL decoration.
//   WHAT     is not expressible in the request. There is no scopes field to send, because
//            internal/sessionkey has no scopes column and internal/auth assigns the set as a
//            constant.
//   HOW LONG is min(asked, configured ceiling, THE CALLER'S OWN REMAINING LIFE) — a session key
//            cannot outlive the sign-in that created it.

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/sessionkey"
)

// sessionKeyMinter is the narrow slice of *sessionkey.Store these routes need.
type sessionKeyMinter interface {
	Mint(ctx context.Context, workspaceID, userID string, ttl time.Duration) (string, *sessionkey.SessionKey, error)
	RevokeAll(ctx context.Context, workspaceID, userID string) (int64, error)
	Revoke(ctx context.Context, workspaceID, id string) error
}

type sessionKeyMintRequest struct {
	TTLSeconds int `json:"ttl_seconds"`
}

type sessionKeyMintResponse struct {
	Key       string `json:"key"`
	ID        string `json:"id"`
	Prefix    string `json:"prefix"`
	ExpiresAt string `json:"expires_at"`
	Warning   string `json:"warning"`
}

// mountSessionKeyRoutes registers the three session-key routes on an ALREADY-AUTHENTICATED router.
//
// ⚠ IT IS CALLED ONLY WHEN THE FEATURE IS CONFIGURED. A deployment that does not enable session
// keys never registers these paths at all, so they are a chi 404 rather than a route that exists
// and refuses — the same posture the H5 flags take, and the reason an unconfigured deployment is
// byte-for-byte unchanged by this feature.
func mountSessionKeyRoutes(r chi.Router, store sessionKeyMinter, ttlCeiling time.Duration) {
	r.Post("/v1/auth/session-keys", newSessionKeyMintHandler(store, ttlCeiling))
	r.Delete("/v1/auth/session-keys", newSessionKeyRevokeAllHandler(store))
	r.Delete("/v1/auth/session-keys/{id}", newSessionKeyRevokeHandler(store))
}

// sessionCaller resolves the browser session behind a request, or reports why it is not one.
//
// ⚠ THE TEST IS AuthMethod == MethodJWT, NOT "has a workspace". A workspace API key has a workspace
// too, and admitting it would mint a "session" key belonging to no session — with no sign-out that
// could ever revoke it, because there is no sign-out for a server-side key.
func sessionCaller(r *http.Request) (*auth.AuthContext, bool) {
	actx := auth.GetAuthContext(r.Context())
	if actx == nil || actx.AuthMethod != auth.MethodJWT || actx.WorkspaceID == "" || actx.UserID == "" {
		return nil, false
	}
	return actx, true
}

func newSessionKeyMintHandler(store sessionKeyMinter, ttlCeiling time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actx, ok := sessionCaller(r)
		if !ok {
			writeJSONErr(w, http.StatusForbidden,
				"forbidden: only a signed-in browser session may mint a session key")
			return
		}
		var in sessionKeyMintRequest
		if r.Body != nil {
			// A body is optional; an unreadable one is not a reason to refuse a credential the
			// caller is entitled to, so a decode failure falls through to the defaults.
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in)
		}
		ttl := clampSessionKeyTTL(time.Duration(in.TTLSeconds)*time.Second, ttlCeiling, actx.ExpiresAt)
		if ttl <= 0 {
			// The caller's own credential is expired or expiring within the second. Minting here
			// would produce a dead key and a confusing 201.
			writeJSONErr(w, http.StatusForbidden, "forbidden: the calling session has no remaining life")
			return
		}
		raw, k, err := store.Mint(r.Context(), actx.WorkspaceID, actx.UserID, ttl)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, sessionKeyMintResponse{
			Key:       raw,
			ID:        k.ID,
			Prefix:    k.KeyPrefix,
			ExpiresAt: k.ExpiresAt.UTC().Format(time.RFC3339),
			Warning:   "Store this key in memory only. It is not shown again and it dies with your session.",
		})
	}
}

// clampSessionKeyTTL applies all three bounds in one place.
//
// ⚠ ONE PLACE ON PURPOSE. internal/sessionkey deliberately does NOT clamp: a clamp in two layers is
// a clamp nobody can state the value of, and the ceiling that matters (the caller's own expiry) is
// only knowable here.
//
// callerExpiry is ZERO for credential shapes that have no lifetime; since only a JWT reaches this
// function, that would mean a token with no exp — which ValidateToken already refuses
// (jwt.WithExpirationRequired). Treating zero as "no remaining life" keeps the failure closed
// rather than open, so a future caller shape cannot silently acquire an unbounded key.
func clampSessionKeyTTL(asked, ceiling time.Duration, callerExpiry time.Time) time.Duration {
	ttl := ceiling
	if asked > 0 && asked < ttl {
		ttl = asked
	}
	remaining := time.Until(callerExpiry)
	if remaining < ttl {
		ttl = remaining
	}
	return ttl
}

func newSessionKeyRevokeAllHandler(store sessionKeyMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actx, ok := sessionCaller(r)
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "forbidden: only a signed-in browser session may revoke its keys")
			return
		}
		n, err := store.RevokeAll(r.Context(), actx.WorkspaceID, actx.UserID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// The COUNT is the point of the response. "Sign-out returned 200" says nothing — 200 is
		// also what deliberately doing nothing looks like.
		writeJSONOK(w, http.StatusOK, map[string]int64{"revoked": n})
	}
}

func newSessionKeyRevokeHandler(store sessionKeyMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actx, ok := sessionCaller(r)
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "forbidden: only a signed-in browser session may revoke its keys")
			return
		}
		// ⚠ THE WORKSPACE GOES INTO THE STORE CALL, which puts it in the WHERE clause. Revoking by
		// id alone would let anyone who learns an id kill another tenant's live chat. The call is a
		// silent no-op for a foreign or absent id, which is also what stops this route being an
		// existence oracle for other tenants' key ids.
		if err := store.Revoke(r.Context(), actx.WorkspaceID, chi.URLParam(r, "id")); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
