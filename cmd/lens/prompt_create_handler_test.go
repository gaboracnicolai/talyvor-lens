package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/prompts"
)

// prompt_create_handler_test.go — POST /v1/prompts writing into somebody else's
// workspace, and the request of theirs it changes.
//
// THE ATTACK, in one line: ws-attacker posts
// {"name":"support","content":"<attacker text>","workspace_id":"ws-victim"} and
// ws-victim's next request carrying the system message `lens:prompt:support` has
// the attacker's text substituted into it before it leaves for the provider.
//
// Both halves are driven, not argued: the HTTP half through the registered
// handler with an attacker credential in the request context, and the consequence
// half through the REAL prompts.Manager and its REAL Resolve — the same call
// proxy.go makes on every request (proxy.go: `p.promptManager.Resolve(ctx, body,
// wsID)`).

// authedReq builds a request carrying the identity AuthMiddleware would have put
// in the context: a non-admin workspace credential, which is what every tenant has.
func authedReq(t *testing.T, method, target, body, workspaceID string, isAdmin bool) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	ctx := auth.WithAuthContext(req.Context(), &auth.AuthContext{
		WorkspaceID: workspaceID,
		IsAdmin:     isAdmin,
	})
	return req.WithContext(ctx)
}

func postPrompt(t *testing.T, h http.HandlerFunc, body, callerWS string, isAdmin bool) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, authedReq(t, http.MethodPost, "/v1/prompts", body, callerWS, isAdmin))
	return rec
}

// ── the property, at the handler seam ──

func TestPromptCreate_NonAdminCannotNameAnotherWorkspace(t *testing.T) {
	mgr := prompts.New(nil) // cache-only Manager: no DB needed for the workspace decision
	h := newPromptCreateHandler(mgr)

	rec := postPrompt(t,
		h,
		`{"name":"support","content":"ATTACKER-TEXT","workspace_id":"ws-victim"}`,
		"ws-attacker", false)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got prompts.Prompt
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	// NON-VACUITY: the create really happened, so "it is not in ws-victim" cannot
	// be true merely because nothing was created.
	if got.Name != "support" || got.Content != "ATTACKER-TEXT" {
		t.Fatalf("nothing was created (name=%q content=%q) — this test would then pass for "+
			"the wrong reason", got.Name, got.Content)
	}
	if got.WorkspaceID != "ws-attacker" {
		t.Errorf("a credential for ws-attacker created a prompt in workspace %q.\n"+
			"    POST /v1/prompts took the workspace from the REQUEST BODY. Its siblings do not: "+
			"PUT /v1/prompts/{name} and POST /v1/prompts/{name}/rollback both call "+
			"effectiveWorkspaceID (#146), and the four GETs call applyPhase2WSID.",
			got.WorkspaceID)
	}
}

func TestPromptCreate_AdminStillHonoursTheBody(t *testing.T) {
	// The mirror. effectiveWorkspaceID means "non-admin forced to own, admin
	// honours the request" everywhere else in this binary, and narrowing the
	// create must not quietly take the admin's ability with it.
	mgr := prompts.New(nil)
	h := newPromptCreateHandler(mgr)

	rec := postPrompt(t, h,
		`{"name":"seeded","content":"OPERATOR-TEXT","workspace_id":"ws-any"}`,
		"", true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got prompts.Prompt
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.WorkspaceID != "ws-any" {
		t.Errorf("admin create landed in %q, want ws-any — the admin's cross-workspace "+
			"create was removed along with the tenant's", got.WorkspaceID)
	}
}

func TestPromptCreate_HonestCallerNamingItsOwnWorkspaceIsUnaffected(t *testing.T) {
	mgr := prompts.New(nil)
	h := newPromptCreateHandler(mgr)

	rec := postPrompt(t, h,
		`{"name":"mine","content":"MY-TEXT","workspace_id":"ws-me"}`,
		"ws-me", false)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got prompts.Prompt
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.WorkspaceID != "ws-me" || got.Content != "MY-TEXT" {
		t.Errorf("an honest caller naming its own workspace got ws=%q content=%q",
			got.WorkspaceID, got.Content)
	}
}

func TestPromptCreate_NoWorkspaceIdentityIsRefused(t *testing.T) {
	mgr := prompts.New(nil)
	h := newPromptCreateHandler(mgr)
	rec := postPrompt(t, h, `{"name":"x","content":"y","workspace_id":"ws-victim"}`, "", false)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a non-admin with no resolvable workspace: status = %d, want 403 — it must not "+
			"fall through to the body's workspace or to \"default\"", rec.Code)
	}
}

// ── THE CONSEQUENCE, through the same call proxy.go makes ──

func TestPromptCreate_DoesNotChangeWhatAnotherTenantsRequestResolvesTo(t *testing.T) {
	ctx := context.Background()
	mgr := prompts.New(nil)

	// The victim has its own prompt under this name.
	if _, err := mgr.Create(ctx, prompts.Prompt{
		Name: "support", Content: "VICTIM-TEXT", WorkspaceID: "ws-victim",
	}); err != nil {
		t.Fatalf("seed victim prompt: %v", err)
	}

	// ARMED: the victim's request really does resolve to the victim's text before
	// anything else happens. Without this the assertion below could pass because
	// resolution never worked at all.
	victimBody := []byte(`{"messages":[{"role":"system","content":"lens:prompt:support"}]}`)
	before, err := mgr.Resolve(ctx, victimBody, "ws-victim")
	if err != nil {
		t.Fatalf("resolve (before): %v", err)
	}
	if !bytes.Contains(before, []byte("VICTIM-TEXT")) {
		t.Fatalf("the victim's own prompt does not resolve, so this test cannot show it being "+
			"replaced. got=%s", before)
	}

	// The attack, through the registered handler with an attacker credential.
	h := newPromptCreateHandler(mgr)
	rec := postPrompt(t, h,
		`{"name":"support","content":"ATTACKER-TEXT","workspace_id":"ws-victim"}`,
		"ws-attacker", false)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusForbidden {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
	}

	// The victim's next request — the exact call proxy.go makes.
	after, err := mgr.Resolve(ctx, victimBody, "ws-victim")
	if err != nil {
		t.Fatalf("resolve (after): %v", err)
	}
	if bytes.Contains(after, []byte("ATTACKER-TEXT")) {
		t.Errorf("ws-attacker changed what ws-victim's request resolves to.\n"+
			"    proxy.go swaps `lens:prompt:<name>` for the stored content of that name in the "+
			"CALLER'S workspace, and prompts.Manager.Get reads an in-memory cache keyed "+
			"(name, workspaceID) before it reads Postgres — which Create writes unconditionally.\n"+
			"    resolved body = %s", after)
	}
	if !bytes.Contains(after, []byte("VICTIM-TEXT")) {
		t.Errorf("the victim's own prompt no longer resolves after the attack: %s", after)
	}
}

// ── the row, on real Postgres ──

func promptsPGHarness(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG prompt create tenancy test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	// Mirrors migration 0010.
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS prompts`,
		`CREATE TABLE prompts (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name         TEXT NOT NULL,
			version      INTEGER NOT NULL DEFAULT 1,
			content      TEXT NOT NULL,
			description  TEXT NOT NULL DEFAULT '',
			workspace_id TEXT NOT NULL DEFAULT 'default',
			is_active    BOOLEAN NOT NULL DEFAULT true,
			created_by   TEXT NOT NULL DEFAULT '',
			created_at   TIMESTAMPTZ DEFAULT NOW(),
			updated_at   TIMESTAMPTZ DEFAULT NOW(),
			UNIQUE(name, version, workspace_id))`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func TestPromptCreate_RowLandsInTheCallersWorkspace_RealPG(t *testing.T) {
	pool := promptsPGHarness(t)
	mgr := prompts.New(pool)
	h := newPromptCreateHandler(mgr)

	rec := postPrompt(t, h,
		`{"name":"support","content":"ATTACKER-TEXT","workspace_id":"ws-victim"}`,
		"ws-attacker", false)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	var ws, content string
	if err := pool.QueryRow(context.Background(),
		`SELECT workspace_id, content FROM prompts WHERE name = 'support'`).Scan(&ws, &content); err != nil {
		t.Fatalf("read row: %v — nothing was written, so the row's workspace is untested", err)
	}
	if content != "ATTACKER-TEXT" {
		t.Fatalf("the row is not the one this test created (content=%q)", content)
	}
	if ws != "ws-attacker" {
		t.Errorf("the persisted row landed in workspace_id=%q; a ws-attacker credential must "+
			"not be able to write a row owned by ws-victim", ws)
	}
}

// ── the wiring ──
//
// Every test above drives newPromptCreateHandler directly, so all of them stay
// green if main.go stops using it. The handler can be perfect and unreached; this
// is the assertion that says so. (The router is built inline inside run()'s
// dependency graph, so the registration is read from the source — the same
// constraint admin_route_classification_test.go documents.)
func TestPromptCreateRouteGoesThroughTheScopedHandler(t *testing.T) {
	src := []byte(readMainGo(t))
	// ⚠ BOTH HALVES WERE RAW TEXT UNTIL #527. The registration was an exact-text Contains, so
	// serving /v1/prompts from an UNSCOPED inline handler while leaving the scoped registration in
	// a COMMENT passed — the workspace scoping in prompt_create_handler.go unreached, exactly the
	// case this assertion exists for. And the bypass rule counted the receiver's SPELLING, so
	// `pm := promptManager` then `pm.Create(…)` walked past it.
	regs, _, _, err := scanRouteRegistrations("main.go", src)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	if len(regs) < 100 {
		t.Fatalf("only %d route registrations parsed from main.go — the scan is blind", len(regs))
	}
	r, ok := findRoute(regs, "Post", "/v1/prompts")
	switch {
	case !ok:
		t.Error("main.go does not register POST /v1/prompts at all.")
	case !r.wrapsCall("newPromptCreateHandler"):
		t.Errorf("main.go registers POST /v1/prompts with handler %s, not newPromptCreateHandler "+
			"(main.go line %d).\n"+
			"    Without it the workspace scoping in prompt_create_handler.go is unreached and "+
			"every other test in this file is driving a handler the binary does not serve.", r.handler, r.line)
	}
	// And nothing else may call Create from main.go — an inline second create path
	// would bypass the scoping exactly the way the original did. Aliases are followed.
	loose, err := callsOnAliasesOf("main.go", src, "promptManager", "Create")
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	if len(loose) > 0 {
		t.Errorf("main.go calls promptManager.Create directly at line(s) %v; the create path must go "+
			"through newPromptCreateHandler so the workspace scoping applies", loose)
	}
}
