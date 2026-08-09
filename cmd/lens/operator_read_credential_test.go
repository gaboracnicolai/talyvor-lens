package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/tenant"
)

// ⚠ A NARROW OPERATOR READ CREDENTIAL — asserted over HTTP, on the real gates.
//
// THE GAP. suite #87 shipped the operator boundary: an (issuer, sub) allowlist in front of six
// cross-tenant reads. Every one of them answers 501, because the only credential Lens's admin
// reads accept is LENS_API_KEY — IsAdmin, which mints LXC, approves conversion rates, revokes
// payouts and mints a token for any tenant. So the operator screen's choice was "no data" or "the
// session gateway holds the keys to the money path". This is the third option.
//
// ⚠ THESE TESTS DRIVE HTTP, NOT THE SCOPE CHECKER. A unit test on HasScope would pass on a
// credential some route nonetheless honours — the property is what the SERVER does with the
// request, so every assertion below is a real gate and a real status code.
//
// ⚠ AND THEY ASSERT THE HANDLER WAS REACHED, NOT JUST THE STATUS. A status code alone cannot tell
// "the gate refused" from "the gate let it through and the handler answered 401", and for a
// security guard those are opposite outcomes.
//
// ⚠ THE CONTROL IS HALF THE TEST. Every refusal is paired with the global key succeeding on the
// same gate. A refusal that also refuses admin is not a narrowing, it is an outage — and it would
// go green here without the pairing.

const testOperatorReadKey = "narrow-operator-read-key-value"

func operatorReadManager(t *testing.T, globalKey, mintKey, operatorKey string) *auth.Manager {
	t.Helper()
	pk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return auth.NewManager(globalKey, pk, nil, nil).
		WithMintKey(mintKey).
		WithOperatorReadKey(operatorKey)
}

func opDefaultManager(t *testing.T) *auth.Manager {
	t.Helper()
	return operatorReadManager(t, testGlobalKey, testMintKey, testOperatorReadKey)
}

// opSentinel records whether the guarded handler was actually entered.
type opSentinel struct{ reached bool }

func (s *opSentinel) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.reached = true
	w.WriteHeader(http.StatusOK)
}

// opCall builds the named gate around a fresh sentinel, drives it, and returns both.
func opCall(t *testing.T, am adminAuthenticator,
	gateFn func(adminAuthenticator, http.Handler) http.HandlerFunc,
	method, path, bearer string,
) (*httptest.ResponseRecorder, *opSentinel) {
	t.Helper()
	s := &opSentinel{}
	gate := gateFn(am, s)
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	gate(rec, req)
	return rec, s
}

// ⚠ THE HEADLINE. The credential reaches every route classified operatorReadable, on GET. Driven
// from the SAME table the router is checked against in admin_route_classification_test.go, so a
// route cannot be classified readable without also being proven reachable.
func TestOperatorReadKey_ReachesEveryReadableRouteOnGET(t *testing.T) {
	am := opDefaultManager(t)
	if len(operatorReadable) == 0 {
		t.Fatal("operatorReadable is empty — this test would pass while granting nothing")
	}
	for path := range operatorReadable {
		t.Run(path, func(t *testing.T) {
			rec, s := opCall(t, am, requireAdminOrOperatorRead, http.MethodGet, path, testOperatorReadKey)
			if rec.Code != http.StatusOK || !s.reached {
				t.Fatalf("GET %s with the operator read credential = %d (reached=%v), want 200 and "+
					"reached. The operator screen cannot render without it, and the alternative is "+
					"the BFF holding LENS_API_KEY.", path, rec.Code, s.reached)
			}
		})
	}
}

// CONTROL: admin reaches the new gate on every verb it reached through requireAdmin before. These
// are r.Handle registrations, so admin genuinely had every verb.
func TestOperatorReadGate_AdminIsUnchanged(t *testing.T) {
	am := opDefaultManager(t)
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete} {
		rec, s := opCall(t, am, requireAdminOrOperatorRead, m, "/v1/admin/workspaces", testGlobalKey)
		if rec.Code != http.StatusOK || !s.reached {
			t.Errorf("ADMIN %s through the new gate = %d (reached=%v), want 200 — the gate must not "+
				"take anything away from the key that already worked", m, rec.Code, s.reached)
		}
	}
}

// FAILS CLOSED, like requireAdmin: no credential, or a wrong one, never reaches the handler.
func TestOperatorReadGate_FailsClosed(t *testing.T) {
	am := opDefaultManager(t)
	for _, bearer := range []string{"", "not-a-key", testOperatorReadKey + "x"} {
		rec, s := opCall(t, am, requireAdminOrOperatorRead, http.MethodGet, "/v1/admin/workspaces", bearer)
		if rec.Code != http.StatusUnauthorized || s.reached {
			t.Errorf("bearer %q = %d (reached=%v), want 401 and NOT reached", bearer, rec.Code, s.reached)
		}
	}
}

// ⚠ TRAP 2, CLOSED. Eight admin routes are registered with `r.Handle`, which mounts EVERY verb,
// and only one gates its own method (`MethodNotAllowed` appears in exactly one non-test file in
// the repository). Granting a PATH would therefore have granted POST, PUT, PATCH and DELETE on it.
func TestOperatorReadKey_MayOnlyRead(t *testing.T) {
	am := opDefaultManager(t)
	const path = "/v1/admin/workspaces" // a real all-verb r.Handle registration

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec, s := opCall(t, am, requireAdminOrOperatorRead, m, path, testOperatorReadKey)
		if rec.Code != http.StatusMethodNotAllowed || s.reached {
			t.Errorf("%s %s with the read credential = %d (reached=%v), want 405 and NOT reached. "+
				"The router mounts every verb on this path; the method check in the gate is the "+
				"only thing between a read credential and a write verb.", m, path, rec.Code, s.reached)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("%s refusal carried Allow=%q, want \"GET, HEAD\" — a 405 with no Allow is a "+
				"dead end for the caller", m, allow)
		}
	}

	// HEAD is a read and is allowed, so the 405s above are about WRITING and not about "any method
	// whose name is not literally GET".
	rec, s := opCall(t, am, requireAdminOrOperatorRead, http.MethodHead, path, testOperatorReadKey)
	if rec.Code != http.StatusOK || !s.reached {
		t.Errorf("HEAD %s = %d (reached=%v), want 200 — HEAD is a read", path, rec.Code, s.reached)
	}
}

// ⚠ EVERY MUST-STAY-UNREACHABLE ROUTE, ONE ASSERTION EACH, written out rather than ranged over a
// table so that losing a table row cannot silently drop an assertion. These drive the PLAIN
// requireAdmin gate — the one those routes are actually registered with — because the risk being
// tested is that adding a scope to the authenticator widened a gate somewhere else.
func TestOperatorReadKey_IsRefusedByEveryWriteRouteGate(t *testing.T) {
	am := opDefaultManager(t)
	for _, path := range []string{
		"/v1/admin/lxc/grant",
		"/v1/admin/conversion-rate/approve",
		"/v1/admin/bonds/bond-1/settle",
		"/v1/admin/distill-royalty/adjudicate",
		"/v1/admin/pool-royalty/adjudicate",
		"/v1/admin/held-mints/traffic/adjudicate",
		"/v1/admin/held-mints/eval_contribution_mints/adjudicate",
		"/v1/admin/held-mints/routing_prediction_mints/adjudicate",
		"/v1/admin/held-mints/node_latency_mints/adjudicate",
		"/v1/admin/held-mints/confidential_compute_mints/adjudicate",
		"/v1/admin/annotation-reputation/reset",
		"/v1/api/injection/patterns",
		"/v1/admin/attest/out-1",
		"/v1/admin/distill/preview",
		"/metrics",
	} {
		t.Run(path, func(t *testing.T) {
			// Neither by a write verb nor by a read one.
			for _, m := range []string{http.MethodPost, http.MethodGet} {
				rec, s := opCall(t, am, requireAdmin, m, path, testOperatorReadKey)
				if rec.Code == http.StatusOK || s.reached {
					t.Fatalf("%s %s ADMITTED the operator read credential (%d, reached=%v). "+
						"Anything that moves money, revokes a payout or changes a price is OUT.",
						m, path, rec.Code, s.reached)
				}
			}
			// ⚠ CONTROL: the global key still reaches it.
			rec, s := opCall(t, am, requireAdmin, http.MethodPost, path, testGlobalKey)
			if rec.Code != http.StatusOK || !s.reached {
				t.Fatalf("%s REFUSED the global admin key (%d) — this change broke admin", path, rec.Code)
			}
		})
	}
}

// ⚠ THE ESCALATION QUESTION THE BRIEF ASKS BY NAME: can it reach POST /v1/auth/token? If it could,
// it would mint itself a token for any workspace and the narrowing would be decorative.
func TestOperatorReadKey_CannotMintAToken(t *testing.T) {
	am := opDefaultManager(t)

	rec := callMint(t, am, testOperatorReadKey, `{"workspace_id":"ws-any","ttl_hours":1}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /v1/auth/token with the operator read credential = %d, want 403. A read "+
			"credential that can mint is not a read credential — it can issue itself a token for "+
			"any workspace. body=%s", rec.Code, rec.Body.String())
	}
	// Naming scopes explicitly must not open a second door.
	if rec := callMint(t, am, testOperatorReadKey, `{"workspace_id":"ws-any","scopes":["admin"]}`); rec.Code != http.StatusForbidden {
		t.Fatalf("a scoped mint request with the read credential = %d, want 403", rec.Code)
	}

	// ⚠ CONTROLS, BOTH SIDES. The route mints for the two credentials that may, so the 403 above is
	// about this credential and not about a mint route that refuses everyone.
	if rec := callMint(t, am, testGlobalKey, `{"workspace_id":"ws-any","ttl_hours":1}`); rec.Code != http.StatusCreated {
		t.Fatalf("ADMIN mint = %d — the refusal above measures nothing if minting is broken. body=%s",
			rec.Code, rec.Body.String())
	}
	if rec := callMint(t, am, testMintKey, `{"workspace_id":"ws-any","ttl_hours":1}`); rec.Code != http.StatusCreated {
		t.Fatalf("MINT-KEY mint = %d, want 201 — adding the read credential broke #402's mint "+
			"credential. body=%s", rec.Code, rec.Body.String())
	}
}

// The resolved identity, asserted directly: not admin, exactly one scope, its own AuthMethod, no
// workspace.
func TestOperatorReadKey_IsNotAdminAndCarriesOnlyOperatorRead(t *testing.T) {
	am := opDefaultManager(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+testOperatorReadKey)
	actx, err := am.Authenticate(req)
	if err != nil {
		t.Fatalf("the operator read credential does not authenticate: %v", err)
	}
	if actx.IsAdmin {
		t.Fatal("the operator read credential is IsAdmin — every requireAdmin gate would admit it")
	}
	if len(actx.Scopes) != 1 || actx.Scopes[0] != auth.ScopeOperatorRead {
		t.Fatalf("scopes = %v, want exactly [%s]", actx.Scopes, auth.ScopeOperatorRead)
	}
	if actx.HasScope(auth.ScopeAdmin) || actx.HasScope(auth.ScopeKeys) ||
		actx.HasScope(auth.ScopeMint) || actx.HasScope(auth.ScopeProxy) || actx.HasScope(auth.ScopeAnalytics) {
		t.Fatalf("it carries more than {operator_read}: %v", actx.Scopes)
	}
	if actx.AuthMethod != auth.MethodOperatorReadKey {
		t.Errorf("AuthMethod = %q, want %q so an incident can tell which credential read the "+
			"finances", actx.AuthMethod, auth.MethodOperatorReadKey)
	}
	if actx.WorkspaceID != "" {
		t.Errorf("WorkspaceID = %q, want empty — it is not a tenant", actx.WorkspaceID)
	}
}

// An unset LENS_OPERATOR_READ_KEY must not create a credential, and the empty-string bearer must
// never match. A deployment that does not set it is unchanged by this feature.
func TestOperatorReadKey_UnsetMeansNoCredentialExists(t *testing.T) {
	am := operatorReadManager(t, testGlobalKey, testMintKey, "") // not configured

	for _, bearer := range []string{"", "anything", testOperatorReadKey} {
		rec, s := opCall(t, am, requireAdminOrOperatorRead, http.MethodGet, "/v1/admin/workspaces", bearer)
		if rec.Code == http.StatusOK || s.reached {
			t.Errorf("bearer %q reached an admin read against an UNSET operator key", bearer)
		}
	}
	// ⚠ CONTROL: with the key unset, admin still reads. "Unchanged" has to include working.
	rec, s := opCall(t, am, requireAdminOrOperatorRead, http.MethodGet, "/v1/admin/workspaces", testGlobalKey)
	if rec.Code != http.StatusOK || !s.reached {
		t.Fatalf("admin got %d on an unconfigured deployment — the feature is not opt-in, it is a "+
			"regression", rec.Code)
	}
}

// ⚠ COLLISION 1: same value as LENS_API_KEY ⇒ ADMIN WINS. A silent demotion would take admin away
// from a deployment that already worked, which is worse than the gap this closes.
func TestOperatorReadKey_SameValueAsGlobalKeyStaysAdmin(t *testing.T) {
	am := operatorReadManager(t, testGlobalKey, testMintKey, testGlobalKey)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+testGlobalKey)
	actx, err := am.Authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !actx.IsAdmin {
		t.Fatal("setting LENS_OPERATOR_READ_KEY to the global key's value DEMOTED admin — a " +
			"misconfiguration must not silently take admin away")
	}
}

// ⚠ COLLISION 2: same value as LENS_MINT_KEY ⇒ MINT WINS, so the operator surface is visibly
// broken rather than silently over-privileged. Neither key is a superset of the other, so one has
// to lose; losing loudly on the read side is the safer half. Asserted so the ordering is a
// decision somebody made rather than an accident of where the branch was pasted.
func TestOperatorReadKey_SameValueAsMintKeyLosesToMint(t *testing.T) {
	am := operatorReadManager(t, testGlobalKey, testMintKey, testMintKey)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+testMintKey)
	actx, err := am.Authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if actx.AuthMethod != auth.MethodMintKey {
		t.Fatalf("AuthMethod = %q, want %q — the mint branch must win the collision",
			actx.AuthMethod, auth.MethodMintKey)
	}
	rec, s := opCall(t, am, requireAdminOrOperatorRead, http.MethodGet, "/v1/admin/workspaces", testMintKey)
	if rec.Code == http.StatusOK || s.reached {
		t.Fatal("the conflated key READ an admin route as the mint credential — the collision must " +
			"cost the read, not grant it")
	}
}

// ⚠ THE GATE'S OWN LOGIC, over AuthContext shapes the Manager will not build today but a future
// change might. A stub authenticator is the only way to ask "what would this gate do with THAT
// identity" — the Manager cannot be made to emit them.
func TestOperatorReadGate_AdmitsOnlyAdminOrTheOperatorReadScope(t *testing.T) {
	for _, c := range []struct {
		name string
		actx *auth.AuthContext
		want int
	}{
		{"admin", &auth.AuthContext{IsAdmin: true}, http.StatusOK},
		{"operator read scope", &auth.AuthContext{Scopes: []string{auth.ScopeOperatorRead}}, http.StatusOK},
		// ⚠ THE ONE THAT MATTERS. tenant.ValidScopes contains "admin", so a workspace API key CAN
		// carry the admin SCOPE while not being IsAdmin. It must not become reachable here.
		{"workspace key carrying the admin scope", &auth.AuthContext{WorkspaceID: "ws-a", Scopes: []string{auth.ScopeAdmin}}, http.StatusUnauthorized},
		// ⚠ THE EMPTY-SCOPE TRAP. auth.RequireScope GRANDFATHERS an empty scope list through every
		// gate it protects, and every JWT minted before scopes existed carries one. This gate must
		// not grandfather: doing so would hand cross-tenant reads to every legacy token.
		{"empty scope list (legacy JWT)", &auth.AuthContext{WorkspaceID: "ws-a"}, http.StatusUnauthorized},
		{"proxy scope", &auth.AuthContext{Scopes: []string{auth.ScopeProxy}}, http.StatusUnauthorized},
		{"mint scope", &auth.AuthContext{Scopes: []string{auth.ScopeMint}}, http.StatusUnauthorized},
		{"nil context", nil, http.StatusUnauthorized},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec, s := opCall(t, opStubAuth{actx: c.actx}, requireAdminOrOperatorRead,
				http.MethodGet, "/v1/admin/workspaces", "irrelevant")
			if rec.Code != c.want {
				t.Fatalf("%s = %d, want %d (reached=%v)", c.name, rec.Code, c.want, s.reached)
			}
			if (c.want == http.StatusOK) != s.reached {
				t.Fatalf("%s: reached=%v but status %d — the status and the handler disagree",
					c.name, s.reached, rec.Code)
			}
		})
	}
}

// ⚠ THE SCOPE MUST NOT BE TENANT-GRANTABLE. tenant.ValidScopes is the closed set a workspace may
// name when it creates an API key. operator_read reads across every workspace, so a tenant that
// could name it could grant itself the operator surface — and that grant would arrive through a
// credential this gate honours.
func TestOperatorReadScope_IsNotTenantGrantable(t *testing.T) {
	if tenant.ValidScopes[auth.ScopeOperatorRead] {
		t.Fatalf("tenant.ValidScopes contains %q — a workspace can now mint itself a key carrying "+
			"the operator read scope, which reads every other workspace's finances",
			auth.ScopeOperatorRead)
	}
	// ⚠ CONTROL: the lookup actually works, so the assertion above is not passing because the
	// helper always says no.
	if !tenant.ValidScopes[auth.ScopeAdmin] {
		t.Fatal("the ValidScopes lookup found neither operator_read NOR admin — it is measuring " +
			"nothing, so the assertion above proves nothing")
	}
}

// opStubAuth returns a fixed AuthContext, so the gate can be asked about identities the Manager
// does not currently produce.
type opStubAuth struct{ actx *auth.AuthContext }

func (s opStubAuth) Authenticate(*http.Request) (*auth.AuthContext, error) { return s.actx, nil }

// ─── containment: what else can a credential that AUTHENTICATES reach? ───────────────────────
//
// ⚠ THE GATE IN THIS FILE IS NOT THE ONLY THING HOLDING THE LINE. Anything that authenticates is
// past AuthMiddleware and onto the whole authenticated route group — including /v1/workspaces/
// {wsID}/... routes that convert LENS, transfer tokens and start a checkout, none of which are
// requireAdmin-gated. What refuses this credential there is workspaceIsolationMiddleware, not
// anything added by W1.3. So it is measured here rather than assumed.
//
// ⚠ AND IT IS MEASURED IN THE PRODUCTION TOPOLOGY. workspaceIsolationMiddleware reads
// chi.URLParam(r, "wsID"). Mount it on a router where the pattern has not been matched yet and
// wsID is "", the check is skipped entirely, and EVERY caller gets through — a guard that passes
// vacuously while looking green. The control below is what makes the result trustworthy: a
// DIFFERENT workspace's credential must be refused on the same route in the same harness, and the
// route's OWN workspace credential must get through.

// opWorkspaceRouter builds the r.Group shape cmd/lens/main.go composes: AuthMiddleware, then
// workspaceIsolationMiddleware, then the {wsID} route.
func opWorkspaceRouter(am *auth.Manager, s http.Handler) chi.Router {
	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(auth.AuthMiddleware(auth.New(nil), am))
		authed.Use(workspaceIsolationMiddleware)
		authed.Post("/v1/workspaces/{wsID}/lxc/convert", s.ServeHTTP)
	})
	return r
}

func opWorkspaceCall(r chi.Router, wsID, bearer string) (*httptest.ResponseRecorder, *opSentinel) {
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+wsID+"/lxc/convert", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec, nil
}

// ⚠ THE CONTAINMENT CLAIM, MEASURED. A credential that owns no workspace is refused by every
// {wsID} route — workspaceAuthorized's `callerWorkspaceID != ""` clause is what does it.
func TestOperatorReadKey_CannotReachAnyWorkspaceRoute(t *testing.T) {
	am := opDefaultManager(t)
	s := &opSentinel{}
	r := opWorkspaceRouter(am, s)

	rec, _ := opWorkspaceCall(r, "ws-someone-else", testOperatorReadKey)
	if rec.Code != http.StatusForbidden || s.reached {
		t.Fatalf("POST /v1/workspaces/ws-someone-else/lxc/convert with the operator read "+
			"credential = %d (reached=%v), want 403 and NOT reached. This route CONVERTS ONE "+
			"TENANT'S LENS TO LXC and is not requireAdmin-gated — workspaceIsolationMiddleware is "+
			"the only thing refusing it. body=%s", rec.Code, s.reached, rec.Body.String())
	}

	// ⚠ NON-VACUITY CONTROL 1: the harness is not simply refusing everyone, and the middleware is
	// not silently skipped. The workspace's OWN credential reaches the same route.
	own := &opSentinel{}
	rOwn := opWorkspaceRouter(am, own)
	tok, err := auth.GenerateToken("ws-someone-else", "u1", []string{auth.ScopeProxy}, am.PrivateKey(), time.Hour)
	if err != nil {
		t.Fatalf("mint a workspace token: %v", err)
	}
	if rec, _ := opWorkspaceCall(rOwn, "ws-someone-else", tok); rec.Code != http.StatusOK || !own.reached {
		t.Fatalf("the workspace's OWN credential got %d (reached=%v) on its own route — the 403 "+
			"above is an outage, not isolation, and this test measures nothing. body=%s",
			rec.Code, own.reached, rec.Body.String())
	}

	// ⚠ NON-VACUITY CONTROL 2: that same workspace token is refused on a DIFFERENT workspace. If
	// this passed, wsID would not be resolving and the middleware would be inert — the exact
	// vacuous-guard shape this comment block exists to rule out.
	other := &opSentinel{}
	rOther := opWorkspaceRouter(am, other)
	if rec, _ := opWorkspaceCall(rOther, "ws-a-different-one", tok); rec.Code != http.StatusForbidden || other.reached {
		t.Fatalf("a ws-someone-else token reached ws-a-different-one (%d, reached=%v) — "+
			"workspaceIsolationMiddleware is not resolving wsID in this harness, so the primary "+
			"assertion above is vacuous", rec.Code, other.reached)
	}
}

// ⚠ THE SCOPE GATE, MEASURED. The credential's scope set is NON-EMPTY and does not contain
// `proxy`, so auth.RequireScope refuses it. This matters because RequireScope GRANDFATHERS an
// empty scope list through — had the credential been built with no scopes, it would have reached
// the proxy surface, and nothing in W1.3's own gate would have said so.
func TestOperatorReadKey_IsRefusedByTheProxyScopeGate(t *testing.T) {
	am := opDefaultManager(t)

	drive := func(bearer string, s *opSentinel) int {
		r := chi.NewRouter()
		r.Group(func(authed chi.Router) {
			authed.Use(auth.AuthMiddleware(auth.New(nil), am))
			authed.With(auth.RequireScope(auth.ScopeProxy)).Post("/v1/proxy/chat", s.ServeHTTP)
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/chat", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	s := &opSentinel{}
	if code := drive(testOperatorReadKey, s); code != http.StatusForbidden || s.reached {
		t.Fatalf("the operator read credential reached a proxy-scoped route: %d (reached=%v), "+
			"want 403. Its scope set must stay NON-EMPTY and must not contain proxy — an empty "+
			"set is grandfathered through every RequireScope gate in the process.", code, s.reached)
	}

	// ⚠ CONTROL: a proxy-scoped credential does reach it, so the 403 is about the scope and not
	// about a route that refuses everyone.
	tok, err := auth.GenerateToken("ws-a", "u1", []string{auth.ScopeProxy}, am.PrivateKey(), time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ok := &opSentinel{}
	if code := drive(tok, ok); code != http.StatusOK || !ok.reached {
		t.Fatalf("a proxy-scoped token got %d (reached=%v) — the refusal above measures nothing",
			code, ok.reached)
	}
}
