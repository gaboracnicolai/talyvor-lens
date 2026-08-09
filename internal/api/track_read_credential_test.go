package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/lens/internal/auth"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────
// WHY TRACK'S READ CREDENTIAL HAS NO CORRECT VALUE — MEASURED, NOT ARGUED.
//
// docker-compose.yaml declares TRACK_LENS_API_KEY and leaves it EMPTY, and the comment there
// gives three reasons. All three are properties of THIS server, so they are asserted here rather
// than trusted: a compose comment nobody can run is prose, and the operator who reads it is being
// asked to take a money-path claim on faith.
//
// Track's reads are GET /v1/api/spend/*?workspace_id=<ws> — one call per workspace, because Track
// is multi-tenant and attributes per-issue cost in every workspace it hosts
// (talyvor-track internal/lensintegration/syncer.go, runOnce → SyncFeatureSpend per workspace ID).
// So the credential in that slot must be able to read ANY workspace. Three candidates exist:
//
//	the MINT key      → 403. TestTrackRead_MintCredentialIsRefused.
//	a tlv_ WORKSPACE key → 200 WITH ANOTHER WORKSPACE'S NUMBERS. TestTrackRead_AWorkspaceKeyIs
//	                     SilentlySubstituted. This is the dangerous one and the reason the slot
//	                     cannot simply be filled.
//	the ADMIN key     → works, and is exactly what #402 exists to keep out of Track.
//
// ⚠ THE SECOND ROW IS THE FINDING. "Lens resolves the workspace from the key" is true and reads
// like a safety property; what it MEANS is that ?workspace_id= is IGNORED, not rejected. A key
// bound to workspace B, asked for workspace A, returns 200 and workspace B's spend — which Track
// would then land on workspace A's issues as their AI cost. A refusal would be safe. A silent
// substitution on the money path is not.
//
// ⚠ AND THE EMPTY SLOT IS NOT INERT BY ITSELF. Track's client omits the Authorization header
// entirely when its key is empty (talyvor-track internal/lensintegration/client.go:107,
// `if c.apiKey != ""`), and its IsConfigured() keys on the URL ALONE
// (client.go:78, `return c != nil && c.lensURL != ""`) — so declaring TRACK_LENS_URL starts the
// syncer with no credential at all. What makes that tick harmless is the LAST test in this file:
// this server refuses an unauthenticated read. If that ever changes, an unauthenticated poller
// starts reading the "default" workspace on Track's behalf every 15 minutes.

const (
	trGlobalKey = "global-admin-key-value"
	trMintKey   = "narrow-mint-key-value"
)

func trManager(t *testing.T) *auth.Manager {
	t.Helper()
	pk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return auth.NewManager(trGlobalKey, pk, nil, nil).WithMintKey(trMintKey)
}

// trGatedRouter mounts the real routes behind the real AuthMiddleware, in the order
// cmd/lens/main.go:2070 composes them. auth.New(nil) is a KeyStore with no database, so no tlv_
// key validates through the fast path and the Manager decides — which is the branch the mint and
// global credentials take in production too.
func trGatedRouter(t *testing.T, s *Server) chi.Router {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.AuthMiddleware(auth.New(nil), trManager(t)))
	s.MountAuthenticated(r)
	return r
}

func trCall(t *testing.T, r chi.Router, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func trRows() *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "request_id", "feature", "issue_id",
		"cost_usd", "input_tokens", "output_tokens", "created_at", "serve_source",
	})
}

func trPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

// The mint credential cannot read spend. It authenticates — it is a valid credential — and then
// resolves to no workspace, so the read is refused rather than served against "default".
func TestTrackRead_MintCredentialIsRefused(t *testing.T) {
	pool := trPool(t)
	// No ExpectQuery: reaching the database at all is the failure.
	s := newServerWithPool(t, pool)
	r := trGatedRouter(t, s)

	rec := trCall(t, r, "/v1/api/spend/by-request?workspace_id=ws-a&days=1", trMintKey)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("the mint credential got %d on /v1/api/spend/by-request, want 403.\n"+
			"    If it now READS, TRACK_LENS_API_KEY could be set to LENS_MINT_KEY's value — and the\n"+
			"    same widening would let a Docs or Track compromise read every tenant's spend.\n"+
			"    body=%s", rec.Code, rec.Body.String())
	}

	// ⚠ CONTROL: the route is not simply broken. The admin credential reaches it AND honours
	// ?workspace_id=, which is the asymmetry the whole finding rests on.
	pool.ExpectQuery(`FROM token_events`).WithArgs("ws-a", 1, nil, 500).WillReturnRows(trRows())
	rec2 := trCall(t, r, "/v1/api/spend/by-request?workspace_id=ws-a&days=1", trGlobalKey)
	if rec2.Code != http.StatusOK {
		t.Fatalf("the admin credential got %d — the refusal above proves nothing if the route "+
			"refuses everyone. body=%s", rec2.Code, rec2.Body.String())
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("admin did not read the workspace it asked for: %v", err)
	}
}

// ⚠ THE DANGEROUS ONE. A workspace-scoped key asked for a DIFFERENT workspace is not refused: the
// parameter is dropped and the key's own workspace is read, under the caller's label.
func TestTrackRead_AWorkspaceKeyIsSilentlySubstituted(t *testing.T) {
	pool := trPool(t)
	// The measurement: the caller asked for ws-a; assert the SQL actually ran for ws-b.
	pool.ExpectQuery(`FROM token_events`).WithArgs("ws-b", 1, nil, 500).WillReturnRows(trRows())
	s := newServerWithPool(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/v1/api/spend/by-request?workspace_id=ws-a&days=1", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: "ws-b"}))
	rec := httptest.NewRecorder()
	newRouter(t, s).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 — the point of this test is that the mismatch is NOT an "+
			"error. If it is now refused, TRACK_LENS_API_KEY became settable to a workspace key "+
			"and the compose comment must be revised. body=%s", rec.Code, rec.Body.String())
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("asked for ws-a and expected ws-b to be read; got: %v\n"+
			"    If the read now follows ?workspace_id=, that is a CROSS-TENANT READ: any workspace "+
			"key could name any workspace.", err)
	}

	// ⚠ CONTROL: the same key asking for its OWN workspace reads its own workspace. Without this,
	// the assertion above would also pass on a server that ignored the parameter by reading
	// nothing, or that always read ws-b.
	pool2 := trPool(t)
	pool2.ExpectQuery(`FROM token_events`).WithArgs("ws-b", 1, nil, 500).WillReturnRows(trRows())
	s2 := newServerWithPool(t, pool2)
	req2 := httptest.NewRequest(http.MethodGet, "/v1/api/spend/by-request?workspace_id=ws-b&days=1", nil)
	req2 = req2.WithContext(auth.WithAuthContext(req2.Context(), &auth.AuthContext{WorkspaceID: "ws-b"}))
	rec2 := httptest.NewRecorder()
	newRouter(t, s2).ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("a workspace key reading its OWN workspace got %d — then the substitution above "+
			"is an outage, not a substitution. body=%s", rec2.Code, rec2.Body.String())
	}
	if err := pool2.ExpectationsWereMet(); err != nil {
		t.Fatalf("own-workspace read did not happen: %v", err)
	}
}

// What keeps an EMPTY TRACK_LENS_API_KEY harmless: Track's client sends no Authorization header,
// and this server refuses that read. The syncer's tick therefore returns an error and writes
// nothing — it is not a read of the "default" workspace attributed to every tenant.
func TestTrackRead_NoCredentialNeverReachesTheData(t *testing.T) {
	pool := trPool(t)
	// No ExpectQuery: any query at all is the failure this test exists to catch.
	s := newServerWithPool(t, pool)
	r := trGatedRouter(t, s)

	rec := trCall(t, r, "/v1/api/spend/by-request?workspace_id=ws-a&days=1", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated spend read got %d, want 401.\n"+
			"    Track's syncer polls this route every 15 minutes with NO Authorization header "+
			"whenever TRACK_LENS_API_KEY is empty (its IsConfigured() keys on the URL alone). If "+
			"this route stops refusing, that poller starts landing another workspace's spend on "+
			"every tenant's issues. body=%s", rec.Code, rec.Body.String())
	}

	// ⚠ CONTROL: the same router serves a credentialed read, so the 401 is about the missing
	// header and not about a router that answers 401 to everything.
	pool.ExpectQuery(`FROM token_events`).WithArgs("ws-a", 1, nil, 500).WillReturnRows(trRows())
	rec2 := trCall(t, r, "/v1/api/spend/by-request?workspace_id=ws-a&days=1", trGlobalKey)
	if rec2.Code != http.StatusOK {
		t.Fatalf("the credentialed control got %d — this router refuses everyone, so the 401 "+
			"above measures nothing. body=%s", rec2.Code, rec2.Body.String())
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("control read did not reach the database: %v", err)
	}
}
