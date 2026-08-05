package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
)

// ⚠ A NARROW MINT CREDENTIAL — asserted over HTTP, on the real routes.
//
// THE GAP. POST /v1/auth/token required actx.IsAdmin, and only the global key (LENS_API_KEY)
// carries that. So Docs and Track — both of which mint per-workspace tokens through lenscreds —
// were obliged to hold a credential bearing ScopeAdmin: LXC grants, royalty adjudication,
// injection patterns, and minting for EVERY tenant. Docs is the largest attack surface in the
// suite (user-authored content, a rich-text editor, embeds, HTML), so a Docs compromise became
// Lens admin over every workspace.
//
// ⚠ THESE TESTS DRIVE HTTP, NOT THE SCOPE CHECKER. A unit test on HasScope would pass on a
// credential that some route nonetheless honours — the property is what the SERVER does with the
// request, so every assertion below is a real handler and a real status code.
//
// ⚠ AND THE CONTROL IS HALF THE TEST. A change that narrows Docs by breaking admin is worse than
// the gap it closes, so every refusal case is paired with the global key succeeding on the same
// route. If the control ever goes red, this feature has cost more than it bought.

func narrowMintManager(t *testing.T, globalKey, mintKey string) *auth.Manager {
	t.Helper()
	pk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return auth.NewManager(globalKey, pk, nil, nil).WithMintKey(mintKey)
}

const (
	testGlobalKey = "global-admin-key-value"
	testMintKey   = "narrow-mint-key-value"
)

func callMint(t *testing.T, am *auth.Manager, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := newAuthTokenMintHandler(am)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/token", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// ⚠ THE HEADLINE. The narrow credential mints a token.
func TestMintKey_CanMintAPerWorkspaceToken(t *testing.T) {
	am := narrowMintManager(t, testGlobalKey, testMintKey)
	rec := callMint(t, am, testMintKey, `{"workspace_id":"ws-docs","ttl_hours":1}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("mint with the narrow credential = %d (%s), want 201. Docs cannot get a "+
			"per-workspace token without it, and the alternative is holding the global admin key.",
			rec.Code, rec.Body.String())
	}
	var out struct{ Token string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("no token in the response: %s", rec.Body.String())
	}
}

// ⚠ THE MINTED TOKEN IS NARROWER THAN THE ONE ADMIN MINTS TODAY. Today's minted tokens carry an
// EMPTY scope list, and RequireScope grandfathers an empty list through every gate. A mint-scoped
// caller gets an explicit {proxy} instead — strictly less.
func TestMintKey_MintedTokenCarriesOnlyProxyScope(t *testing.T) {
	am := narrowMintManager(t, testGlobalKey, testMintKey)
	rec := callMint(t, am, testMintKey, `{"workspace_id":"ws-docs","ttl_hours":1}`)
	var out struct{ Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+out.Token)
	actx, err := am.Authenticate(req)
	if err != nil {
		t.Fatalf("the minted token does not authenticate: %v", err)
	}
	if actx.IsAdmin {
		t.Fatal("the minted token is ADMIN — a mint credential minted itself full privilege")
	}
	if len(actx.Scopes) != 1 || actx.Scopes[0] != auth.ScopeProxy {
		t.Fatalf("minted scopes = %v, want exactly [%s]", actx.Scopes, auth.ScopeProxy)
	}
}

// ⚠ THE ESCALATION PATH, CLOSED. The mint body carries `scopes`; without this a compromised Docs
// mints itself an admin token and the narrowing is decorative.
func TestMintKey_CannotChooseItsOwnTokenScopes(t *testing.T) {
	am := narrowMintManager(t, testGlobalKey, testMintKey)
	for _, body := range []string{
		`{"workspace_id":"ws-docs","scopes":["admin"]}`,
		`{"workspace_id":"ws-docs","scopes":["keys"]}`,
		`{"workspace_id":"ws-docs","scopes":["proxy"]}`, // even the one it gets anyway
	} {
		rec := callMint(t, am, testMintKey, body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("mint with %s = %d, want 403. A minter that can name its token's scopes can "+
				"mint itself admin.", body, rec.Code)
		}
	}
}

// CONTROL: an admin caller keeps today's behaviour, scopes and all.
func TestMintKey_AdminMintIsUnchanged(t *testing.T) {
	am := narrowMintManager(t, testGlobalKey, testMintKey)
	rec := callMint(t, am, testGlobalKey, `{"workspace_id":"ws-x","scopes":["admin","keys"],"ttl_hours":1}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ADMIN mint = %d (%s), want 201 — narrowing Docs must not change what admin can do",
			rec.Code, rec.Body.String())
	}
	var out struct{ Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+out.Token)
	actx, _ := am.Authenticate(req)
	if len(actx.Scopes) != 2 {
		t.Errorf("admin-minted scopes = %v, want the two it asked for", actx.Scopes)
	}
}

// An unset LENS_MINT_KEY must not create a credential. The empty-string bearer must never match.
func TestMintKey_UnsetMeansNoCredentialExists(t *testing.T) {
	am := narrowMintManager(t, testGlobalKey, "") // mint key not configured
	if rec := callMint(t, am, "", `{"workspace_id":"ws"}`); rec.Code != http.StatusForbidden {
		t.Errorf("an empty bearer against an unset mint key = %d, want 403", rec.Code)
	}
	if rec := callMint(t, am, "anything", `{"workspace_id":"ws"}`); rec.Code != http.StatusForbidden {
		t.Errorf("an arbitrary bearer against an unset mint key = %d, want 403", rec.Code)
	}
}

// ⚠ EVERY ADMIN ROUTE FAMILY REFUSES THE MINT CREDENTIAL — one per family, not one for "admin".
//
// Adding a scope to the authenticator is exactly where an accidental grant happens: a route that
// gated on "has any credential" or on a scope rather than IsAdmin would now admit Docs. These
// drive requireAdmin — the gate all 29 admin routes share — with each family's real handler
// replaced by a sentinel, so a 200 means the GATE let it through.
func TestMintKey_EveryAdminRouteFamilyRefusesIt(t *testing.T) {
	am := narrowMintManager(t, testGlobalKey, testMintKey)
	families := []struct{ name, path string }{
		{"lxc grant", "/v1/admin/lxc/grant"},
		{"royalty adjudicate", "/v1/admin/distill-royalty/adjudicate"},
		{"pool royalty adjudicate", "/v1/admin/pool-royalty/adjudicate"},
		{"injection patterns", "/v1/api/injection/patterns"},
		{"economy flags", "/v1/admin/economy/flags"},
		{"keel findings", "/v1/admin/keel/findings"},
		{"workspaces", "/v1/admin/workspaces"},
		{"held mints", "/v1/admin/held-mints/"},
		{"billing purchases", "/v1/admin/billing/purchases"},
		{"conversion-rate approve", "/v1/admin/conversion-rate/approve"},
		{"attest", "/v1/admin/attest/out-1"},
		{"metrics", "/metrics"},
	}
	reached := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			gated := requireAdmin(am, http.HandlerFunc(reached))

			// The narrow credential must be refused.
			req := httptest.NewRequest(http.MethodPost, f.path, nil)
			req.Header.Set("Authorization", "Bearer "+testMintKey)
			rec := httptest.NewRecorder()
			gated(rec, req)
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
				t.Fatalf("%s ADMITTED the mint credential (%d). Adding ScopeMint widened an admin "+
					"route — a Docs compromise now reaches it.", f.path, rec.Code)
			}

			// ⚠ CONTROL: the global key still reaches it. A refusal that also refuses admin is not
			// a narrowing, it is an outage.
			req2 := httptest.NewRequest(http.MethodPost, f.path, nil)
			req2.Header.Set("Authorization", "Bearer "+testGlobalKey)
			rec2 := httptest.NewRecorder()
			gated(rec2, req2)
			if rec2.Code != http.StatusOK {
				t.Fatalf("%s REFUSED the global admin key (%d) — this change broke admin", f.path, rec2.Code)
			}
		})
	}
}

// The mint credential must not be admin anywhere the code asks the question directly.
func TestMintKey_IsNotAdminAndCarriesOnlyMint(t *testing.T) {
	am := narrowMintManager(t, testGlobalKey, testMintKey)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+testMintKey)
	actx, err := am.Authenticate(req)
	if err != nil {
		t.Fatalf("mint credential does not authenticate: %v", err)
	}
	if actx.IsAdmin {
		t.Fatal("the mint credential is IsAdmin — every requireAdmin gate would admit it")
	}
	if actx.HasScope(auth.ScopeAdmin) || actx.HasScope(auth.ScopeKeys) || actx.HasScope(auth.ScopeAnalytics) {
		t.Fatalf("mint credential carries more than {mint}: %v", actx.Scopes)
	}
	if !actx.HasScope(auth.ScopeMint) {
		t.Fatal("mint credential does not carry ScopeMint")
	}
	if actx.AuthMethod != auth.MethodMintKey {
		t.Errorf("AuthMethod = %q, want %q so an incident can tell the two credentials apart",
			actx.AuthMethod, auth.MethodMintKey)
	}
}

// If an operator sets both variables to the same string, admin must WIN — a silent demotion would
// break admin, which is worse than the gap this closes.
func TestMintKey_SameValueAsGlobalKeyStaysAdmin(t *testing.T) {
	am := narrowMintManager(t, testGlobalKey, testGlobalKey)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+testGlobalKey)
	actx, err := am.Authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !actx.IsAdmin {
		t.Fatal("setting LENS_MINT_KEY to the global key's value DEMOTED admin — a misconfiguration " +
			"must not silently take admin away")
	}
}
