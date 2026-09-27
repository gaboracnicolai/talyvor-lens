package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/workspace"
)

// B18.6 — NO WORKSPACE CAN HAVE THE PROMPT REWRITER ON. PUT .../compression refuses every policy but
// "disabled" with 410 and a plain message, and a registration asking for it stores "disabled".
func TestB186_TheRewriterCannotBeTurnedOn(t *testing.T) {
	ctx := context.Background()
	wsm := workspace.New(nil)
	if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{ID: "ws-1", Name: "ws-1", Active: true}); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Put("/v1/workspaces/{wsID}/compression", newCompressionPolicyHandler(wsm))
	put := func(policy string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/workspaces/ws-1/compression",
			strings.NewReader(`{"compression_policy":"`+policy+`"}`)))
		return rec
	}
	for _, p := range []string{"always", "opt_in"} {
		if rec := put(p); rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "prompt rewriter is retired") {
			t.Errorf("PUT %q: %d %s — want 410 saying the rewriter is retired", p, rec.Code, rec.Body.String())
		}
	}
	if rec := put("disabled"); rec.Code != http.StatusOK {
		t.Errorf("PUT disabled: %d %s — turning it off must still succeed", rec.Code, rec.Body.String())
	}
	if got := wsm.GetCompressionPolicy("ws-1"); got != workspace.CompressionDisabled {
		t.Errorf("ws-1 compression policy = %q after the refused PUTs, want disabled", got)
	}

	reg := httptest.NewRecorder()
	newRegisterWorkspaceHandler(wsm).ServeHTTP(reg, httptest.NewRequest(http.MethodPost, "/v1/workspaces",
		strings.NewReader(`{"id":"ws-2","name":"ws-2","active":true,"compression_policy":"always"}`)))
	if reg.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", reg.Code, reg.Body.String())
	}
	if got := wsm.GetCompressionPolicy("ws-2"); got != workspace.CompressionDisabled {
		t.Errorf("a workspace registered with compression_policy always has %q, want disabled", got)
	}
}
