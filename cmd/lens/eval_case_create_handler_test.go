package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talyvor/lens/internal/eval"
)

// eval_case_create_handler_test.go — who may create an eval test case in which
// workspace.
//
// The consequence half lives in internal/eval/planted_case_execution_test.go and
// is executed there against real Postgres: RunSuite(ctx, workspaceID, tags) runs
// EVERY case in that workspace, calling the provider with the model and prompt the
// STORED CASE carries, on the operator's provider key, and costing the result into
// the victim's RunSummary. So whoever can write a case into a workspace chooses
// what that workspace's next eval run pays for.
//
// This file is the write side: a ws-attacker credential must not be able to put a
// case into ws-victim.

type fakeEvalCaseCreator struct {
	got eval.TestCase
}

func (f *fakeEvalCaseCreator) AddTestCase(_ context.Context, tc eval.TestCase) (*eval.TestCase, error) {
	f.got = tc
	return &tc, nil
}

func postEvalCase(t *testing.T, h http.HandlerFunc, body, callerWS string, isAdmin bool) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, authedReq(t, http.MethodPost, "/v1/eval/cases", body, callerWS, isAdmin))
	return rec
}

func TestEvalCaseCreate_NonAdminCannotNameAnotherWorkspace(t *testing.T) {
	f := &fakeEvalCaseCreator{}
	h := newEvalCaseCreateHandler(f)

	rec := postEvalCase(t, h,
		`{"name":"planted","workspace_id":"ws-victim","provider":"openai","model":"gpt-4o","prompt":"ATTACKER-CHOSEN"}`,
		"ws-attacker", false)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	// NON-VACUITY: a case really reached the store, so "it is not in ws-victim"
	// cannot be true merely because nothing was created.
	if f.got.Name != "planted" || f.got.Prompt != "ATTACKER-CHOSEN" {
		t.Fatalf("nothing reached the store (name=%q prompt=%q)", f.got.Name, f.got.Prompt)
	}
	if f.got.WorkspaceID != "ws-attacker" {
		t.Errorf("a credential for ws-attacker created an eval case in workspace %q.\n"+
			"    POST /v1/eval/cases took the workspace from the REQUEST BODY. RunSuite then runs "+
			"every case in that workspace against the provider and model the case names, on the "+
			"operator's key, costed to that workspace's run.",
			f.got.WorkspaceID)
	}
}

func TestEvalCaseCreate_AdminStillHonoursTheBody(t *testing.T) {
	f := &fakeEvalCaseCreator{}
	h := newEvalCaseCreateHandler(f)
	rec := postEvalCase(t, h, `{"name":"seeded","workspace_id":"ws-any","provider":"openai","model":"gpt-4o"}`, "", true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if f.got.WorkspaceID != "ws-any" {
		t.Errorf("admin create landed in %q, want ws-any — the admin's cross-workspace seed was "+
			"removed along with the tenant's", f.got.WorkspaceID)
	}
}

func TestEvalCaseCreate_HonestCallerIsUnaffected(t *testing.T) {
	f := &fakeEvalCaseCreator{}
	h := newEvalCaseCreateHandler(f)
	rec := postEvalCase(t, h,
		`{"name":"mine","workspace_id":"ws-me","provider":"openai","model":"gpt-4o","prompt":"MINE"}`,
		"ws-me", false)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if f.got.WorkspaceID != "ws-me" || f.got.Prompt != "MINE" {
		t.Errorf("honest caller got ws=%q prompt=%q", f.got.WorkspaceID, f.got.Prompt)
	}
}

func TestEvalCaseCreate_NoWorkspaceIdentityIsRefused(t *testing.T) {
	f := &fakeEvalCaseCreator{}
	h := newEvalCaseCreateHandler(f)
	rec := postEvalCase(t, h, `{"name":"x","workspace_id":"ws-victim","provider":"openai","model":"gpt-4o"}`, "", false)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a non-admin with no resolvable workspace: status = %d, want 403 — it must not "+
			"fall through to the body's workspace or to \"default\"", rec.Code)
	}
}

// ── the row, on real Postgres, through the real Pipeline ──

func evalCasePGHarness(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG eval case tenancy test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	// Mirrors migration 0012's eval_test_cases.
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS eval_test_cases`,
		`CREATE TABLE eval_test_cases (
			id              TEXT PRIMARY KEY,
			name            TEXT NOT NULL,
			workspace_id    TEXT NOT NULL DEFAULT 'default',
			provider        TEXT NOT NULL,
			model           TEXT NOT NULL,
			prompt          TEXT NOT NULL,
			expected_output TEXT NOT NULL DEFAULT '',
			eval_method     TEXT NOT NULL DEFAULT 'heuristic',
			pass_threshold  FLOAT NOT NULL DEFAULT 0.6,
			tags            TEXT[] NOT NULL DEFAULT '{}',
			created_at      TIMESTAMPTZ DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func TestEvalCaseCreate_RowLandsInTheCallersWorkspace_RealPG(t *testing.T) {
	pool := evalCasePGHarness(t)
	pipeline := eval.New(pool, nil, "", "", "")
	h := newEvalCaseCreateHandler(pipeline)

	rec := postEvalCase(t, h,
		`{"name":"planted","workspace_id":"ws-victim","provider":"openai","model":"gpt-4o","prompt":"ATTACKER-CHOSEN"}`,
		"ws-attacker", false)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	var ws, prompt string
	if err := pool.QueryRow(context.Background(),
		`SELECT workspace_id, prompt FROM eval_test_cases WHERE name = 'planted'`).Scan(&ws, &prompt); err != nil {
		t.Fatalf("read row: %v — nothing was persisted, so the row's workspace is untested", err)
	}
	if prompt != "ATTACKER-CHOSEN" {
		t.Fatalf("the row is not the one this test created (prompt=%q)", prompt)
	}
	if ws != "ws-attacker" {
		t.Errorf("the persisted case landed in workspace_id=%q; a ws-attacker credential must not "+
			"be able to put a case into ws-victim's suite", ws)
	}
}

// ── the wiring ──
//
// Every test above drives newEvalCaseCreateHandler directly, so all of them stay
// green if main.go stops using it.
func TestEvalCaseCreateRouteGoesThroughTheScopedHandler(t *testing.T) {
	src := readMainGo(t)
	const want = `authed.Post("/v1/eval/cases", newEvalCaseCreateHandler(evalPipeline))`
	if !strings.Contains(src, want) {
		t.Errorf("main.go does not register POST /v1/eval/cases through newEvalCaseCreateHandler; " +
			"the scoping is unreached and every other test in this file drives a handler the " +
			"binary does not serve")
	}
	if n := strings.Count(src, "evalPipeline.AddTestCase("); n != 0 {
		t.Errorf("main.go calls evalPipeline.AddTestCase directly %d time(s); the create path must "+
			"go through newEvalCaseCreateHandler so effectiveWorkspaceID applies", n)
	}
}

// The census guard's own population, kept honest: this file exists because
// POST /v1/eval/cases was in the W6.13 residue — `authed`, no {wsID}, and no
// identity decision reachable from the registration site. It is no longer.
func TestEvalCaseCreateReachesAnIdentityDecision(t *testing.T) {
	src, err := os.ReadFile("eval_case_create_handler.go")
	if err != nil {
		t.Fatalf("read handler: %v", err)
	}
	if !strings.Contains(string(src), "effectiveWorkspaceID(req,") {
		t.Error("newEvalCaseCreateHandler no longer calls effectiveWorkspaceID — the route is back " +
			"in the residue: authed, no {wsID}, no identity decision")
	}
}
