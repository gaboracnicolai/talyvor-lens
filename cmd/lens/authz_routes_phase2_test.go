package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/talyvor/lens/internal/eval"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/povi"
	"github.com/talyvor/lens/internal/prompts"
	"github.com/talyvor/lens/internal/session"
)

// authz_routes_phase2_test.go — behavioral wiring guards for the #146 Phase-2
// sensitive cross-tenant READS, over HTTP against the real handlers, asserting
// against the downstream dependency (the Phase-1 standard). A tenant-A
// credential naming tenant B must read its OWN workspace; no param must mean own
// (never the shared "default"); admin honors the param.

type fakeSessionLister struct{ ws string }

func (f *fakeSessionLister) ListActiveByWorkspace(ws string) []*session.Session {
	f.ws = ws
	return nil
}

type fakeEvalRunsLister struct{ ws string }

func (f *fakeEvalRunsLister) ListRuns(_ context.Context, ws string, _ int) ([]eval.RunSummary, error) {
	f.ws = ws
	return nil, nil
}

type fakePoviReceiptsLister struct{ ws string }

func (f *fakePoviReceiptsLister) ListReceipts(_ context.Context, ws string, _ int) ([]povi.StoredReceipt, error) {
	f.ws = ws
	return nil, nil
}

type fakePromptGetter struct{ ws, name string }

func (f *fakePromptGetter) Get(_ context.Context, name, ws string) (*prompts.Prompt, error) {
	f.name, f.ws = name, ws
	return &prompts.Prompt{}, nil
}

func TestAuthzP2_Sessions_Wiring(t *testing.T) {
	f := &fakeSessionLister{}
	h := newSessionsListHandler(f)
	serveAuthed(t, http.MethodGet, "/v1/sessions", "/v1/sessions?workspace_id=ws-B", "", "ws-A", false, h)
	if f.ws != "ws-A" {
		t.Fatalf("LEAK: sessions listed for %q, want ws-A", f.ws)
	}
	f.ws = ""
	serveAuthed(t, http.MethodGet, "/v1/sessions", "/v1/sessions", "", "ws-A", false, h)
	if f.ws != "ws-A" {
		t.Fatalf("DEFAULT-FALLBACK LEAK: no-param non-admin listed for %q, want ws-A (never \"default\")", f.ws)
	}
	f.ws = ""
	serveAuthed(t, http.MethodGet, "/v1/sessions", "/v1/sessions?workspace_id=ws-B", "", "ws-admin", true, h)
	if f.ws != "ws-B" {
		t.Fatalf("admin: listed for %q, want ws-B (honored)", f.ws)
	}
}

func TestAuthzP2_EvalRuns_Wiring(t *testing.T) {
	f := &fakeEvalRunsLister{}
	h := newEvalRunsListHandler(f)
	serveAuthed(t, http.MethodGet, "/v1/eval/runs", "/v1/eval/runs?workspace_id=ws-B", "", "ws-A", false, h)
	if f.ws != "ws-A" {
		t.Fatalf("LEAK: eval runs for %q, want ws-A", f.ws)
	}
	f.ws = ""
	serveAuthed(t, http.MethodGet, "/v1/eval/runs", "/v1/eval/runs", "", "ws-A", false, h)
	if f.ws != "ws-A" {
		t.Fatalf("no-param non-admin → %q, want ws-A (never default)", f.ws)
	}
	f.ws = ""
	serveAuthed(t, http.MethodGet, "/v1/eval/runs", "/v1/eval/runs?workspace_id=ws-B", "", "ws-admin", true, h)
	if f.ws != "ws-B" {
		t.Fatalf("admin: %q, want ws-B", f.ws)
	}
}

func TestAuthzP2_PoviReceipts_Wiring(t *testing.T) {
	f := &fakePoviReceiptsLister{}
	h := newPoviReceiptsHandler(f)
	// NOTE: this route keys on "workspace" (not "workspace_id").
	serveAuthed(t, http.MethodGet, "/v1/povi/receipts", "/v1/povi/receipts?workspace=ws-B", "", "ws-A", false, h)
	if f.ws != "ws-A" {
		t.Fatalf("LEAK: receipts for %q, want ws-A", f.ws)
	}
	f.ws = ""
	serveAuthed(t, http.MethodGet, "/v1/povi/receipts", "/v1/povi/receipts", "", "ws-A", false, h)
	if f.ws != "ws-A" {
		t.Fatalf("no-param non-admin → %q, want ws-A (never default)", f.ws)
	}
	f.ws = ""
	serveAuthed(t, http.MethodGet, "/v1/povi/receipts", "/v1/povi/receipts?workspace=ws-B", "", "ws-admin", true, h)
	if f.ws != "ws-B" {
		t.Fatalf("admin: %q, want ws-B", f.ws)
	}
}

func TestAuthzP2_GuardrailsGet_Wiring(t *testing.T) {
	eng := guardrails.New(pii.New(), injection.New(injection.DefaultPolicy()))
	eng.SetPolicy(context.Background(), "ws-B", guardrails.GuardrailPolicy{WorkspaceID: "ws-B", BlockedWords: []string{"secret-B"}})
	h := newGuardrailsPolicyGetHandler(eng)

	// ATTACK: ws-A reads ?workspace_id=ws-B → must NOT see ws-B's policy.
	rec := serveAuthed(t, http.MethodGet, "/v1/guardrails/policy", "/v1/guardrails/policy?workspace_id=ws-B", "", "ws-A", false, h)
	var got guardrails.GuardrailPolicy
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if reflect.DeepEqual(got.BlockedWords, []string{"secret-B"}) {
		t.Fatalf("LEAK: ws-A read ws-B's guardrail policy (BlockedWords=%v)", got.BlockedWords)
	}
	// ADMIN may read ws-B.
	rec = serveAuthed(t, http.MethodGet, "/v1/guardrails/policy", "/v1/guardrails/policy?workspace_id=ws-B", "", "ws-admin", true, h)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !reflect.DeepEqual(got.BlockedWords, []string{"secret-B"}) {
		t.Fatalf("admin should read ws-B's policy, got BlockedWords=%v", got.BlockedWords)
	}
}

func TestAuthzP2_PromptGet_Wiring(t *testing.T) {
	f := &fakePromptGetter{}
	h := newPromptGetHandler(f)
	serveAuthed(t, http.MethodGet, "/v1/prompts/{name}", "/v1/prompts/p1?workspace_id=ws-B", "", "ws-A", false, h)
	if f.ws != "ws-A" {
		t.Fatalf("LEAK: prompt fetched for workspace %q, want ws-A", f.ws)
	}
	if f.name != "p1" {
		t.Fatalf("prompt name=%q, want p1", f.name)
	}
	f.ws = ""
	serveAuthed(t, http.MethodGet, "/v1/prompts/{name}", "/v1/prompts/p1", "", "ws-A", false, h)
	if f.ws != "ws-A" {
		t.Fatalf("no-param non-admin → %q, want ws-A (never default)", f.ws)
	}
	f.ws = ""
	serveAuthed(t, http.MethodGet, "/v1/prompts/{name}", "/v1/prompts/p1?workspace_id=ws-B", "", "ws-admin", true, h)
	if f.ws != "ws-B" {
		t.Fatalf("admin: fetched for %q, want ws-B", f.ws)
	}
}
