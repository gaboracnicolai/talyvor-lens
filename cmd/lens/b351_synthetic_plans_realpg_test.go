package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	stripe "github.com/stripe/stripe-go/v81"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/plans"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/workspace"
)

// B35.1 — the testers put a synthetic workspace on any plan, through the synthetic routes on a migrated schema:
// on Team it creates its 25th agent and is refused its 26th, and its model call writes a 3% platform_fee row; on
// Business it creates a 26th; moved back to Free it is refused another; on Enterprise it still takes no live
// money; Stripe is never called; and a workspace that is not synthetic is refused and keeps its plan.
func TestB351_ASyntheticWorkspaceIsOnThePlanTheTestersPutItOn(t *testing.T) {
	pool := syntheticDB(t)
	ctx := context.Background()

	var stripeCalls atomic.Int64
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		stripeCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(stub.Close)
	prev := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(stub.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prev) })

	wsm := workspace.New(pool)
	dual := economy.NewDualTokenStore(nil, pool, nil)
	dual.SetPlatformFee(func(ctx context.Context, q economy.FeeQuerier, ws string) (int64, error) {
		return billing.PlatformFeeBPS(ctx, q, ws, fees.Defaults())
	})
	r := chi.NewRouter()
	mountSyntheticRoutes(r, "the-key", syntheticDeps{workspaces: wsm, credits: dual, answers: storedanswers.New(pool, nil),
		audit: pool, plans: pool,
		mint: func(workspaceID, _ string, _ []string, _ time.Duration) (string, error) { return "tok-" + workspaceID, nil }})
	call := func(path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set(syntheticKeyHeader, "the-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	moveTo := func(ws, plan string) {
		t.Helper()
		code, out := call("/v1/synthetic/workspaces/"+ws+"/plan", `{"plan":"`+plan+`"}`)
		if on, _ := out["plan"].(map[string]any); code != http.StatusOK || on["plan"] != plan {
			t.Fatalf("moving %s to %s = %d %v, want 200 on %s", ws, plan, code, out, plan)
		}
	}
	agents := 0
	newAgent := func(ws string) error {
		agents++
		_, err := dual.CreateAgent(ctx, ws, fmt.Sprintf("agent %d", agents), "owner-"+ws)
		return err
	}
	refusedOn := func(err error, plan string) bool {
		var ref *plans.Refusal
		return errors.As(err, &ref) && ref.Gate == "agents" && ref.Plan == plan
	}

	code, out := call("/v1/synthetic/workspaces", `{"count":1,"plan":"team"}`)
	users, _ := out["workspaces"].([]any)
	if code != http.StatusCreated || out["plan"] != "team" || len(users) != 1 {
		t.Fatalf("create on team = %d %v, want 201 with one workspace on team", code, out)
	}
	ws := users[0].(map[string]any)["workspace_id"].(string)

	for i := 1; i <= 25; i++ {
		if err := newAgent(ws); err != nil {
			t.Fatalf("Team's agent %d: %v", i, err)
		}
	}
	if err := newAgent(ws); !refusedOn(err, plans.Team) {
		t.Fatalf("Team's 26th agent = %v, want refused by the team plan's agents gate", err)
	}
	if _, err := dual.SpendLXCMeta(economy.WithChargeRequest(ctx, "req-b351"), ws, 10_000_000, "chat: metered usage", nil); err != nil {
		t.Fatal(err)
	}
	var fee, bps int64
	if err := pool.QueryRow(ctx, `SELECT amount::bigint, (metadata->>'platform_fee_bps')::bigint FROM lxc_ledger
		WHERE workspace_id = $1 AND type = 'platform_fee'`, ws).Scan(&fee, &bps); err != nil || fee != -300_000 || bps != 300 {
		t.Fatalf("the 10 LXC call's platform_fee row = %d at %d bps (%v), want −300,000 at Team's 300", fee, bps, err)
	}

	moveTo(ws, plans.Business)
	if err := newAgent(ws); err != nil {
		t.Fatalf("Business's 26th agent: %v", err)
	}
	moveTo(ws, plans.Free)
	if err := newAgent(ws); !refusedOn(err, plans.Free) {
		t.Fatalf("an agent after moving back to Free = %v, want refused by the free plan's agents gate", err)
	}
	moveTo(ws, plans.Enterprise)
	if p, err := plans.Of(ctx, pool, ws); err != nil || p.Plan != plans.Enterprise || !p.Edge || p.LiveMoney {
		t.Fatalf("on Enterprise = %+v, %v; want enterprise's gates with live money off", p, err)
	}

	if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{ID: "ws-b351-real", Name: "Real", Active: true}); err != nil {
		t.Fatal(err)
	}
	if code, out := call("/v1/synthetic/workspaces/ws-b351-real/plan", `{"plan":"enterprise"}`); code != http.StatusForbidden {
		t.Fatalf("a real workspace's plan = %d %v, want 403", code, out)
	}
	if p, err := plans.Of(ctx, pool, "ws-b351-real"); err != nil || p.Plan != plans.Free {
		t.Fatalf("the real workspace is on %+v, %v after the refusal; want free, unchanged", p, err)
	}
	if n := stripeCalls.Load(); n != 0 {
		t.Fatalf("the Stripe stub saw %d requests, want none", n)
	}
}
