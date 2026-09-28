package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/distillattrib"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/opsusage"
	"github.com/talyvor/lens/internal/routedecision"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/workspace"
)

// B17.7 — after 100 synthetic workspaces ask questions, every admin total reads exactly what it read
// before, and the ?synthetic=only view shows their traffic. Real handlers, migrated schema.
func TestSyntheticTraffic_LeavesEveryAdminTotalUnchanged(t *testing.T) {
	pool := syntheticDB(t)
	ctx := context.Background()
	ws := workspace.New(pool)

	// What the proxy writes when a workspace asks a question: its token_events row and, for an
	// auto-routed request, its routing decision.
	ask := func(wsID, source string, costUSD float64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO token_events (provider, model, input_tokens, output_tokens, cost_usd, workspace_id, serve_source)
			VALUES ('anthropic', 'claude-haiku-4-5', 120, 40, $1, $2, $3)`, costUSD, wsID, source); err != nil {
			t.Fatalf("token_events: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO routing_decisions (workspace_id, baseline_model, actual_model, cohort_overrode,
			input_tokens, output_tokens, actual_cost_u, counterfactual_cost_estimate_u, cost_basis)
			VALUES ($1, 'gpt-4o', 'claude-haiku-4-5', true, 120, 40, 900, 2400, 'cache_aware')`, wsID); err != nil {
			t.Fatalf("routing_decisions: %v", err)
		}
	}
	shareConversion := func(owner, requester string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO distill_serve_attribution (owner_workspace_id, requester_workspace_id, content_hash, serve_count)
			VALUES ($1, $2, 'h-'||$1, 3)`, owner, requester); err != nil {
			t.Fatalf("distill_serve_attribution: %v", err)
		}
	}

	for _, id := range []string{"real-a", "real-b"} {
		if err := ws.RegisterWorkspace(ctx, workspace.Workspace{ID: id, Name: id, Active: true}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	ask("real-a", "upstream", 0.0125)
	ask("real-a", "cache_hit_exact", 0)
	ask("real-b", "cache_hit_pooled", 0)
	shareConversion("real-a", "real-b")

	r := chi.NewRouter()
	r.Get("/v1/admin/usage/summary", newAdminUsageSummaryHandler(opsusage.NewReader(pool), time.Now))
	r.Get("/v1/admin/workspaces", newAdminListWorkspacesHandler(ws))
	r.Get("/v1/admin/routing-decisions/summary", newRoutingDecisionsSummaryHandler(routedecision.NewReader(pool), time.Now))
	r.Get("/v1/admin/distill/attribution", newDistillAttributionAdminHandler(distillattrib.NewReader(pool)))
	mountSyntheticRoutes(r, "the-key", syntheticDeps{
		workspaces: ws,
		credits:    economy.NewDualTokenStore(nil, pool, nil),
		answers:    storedanswers.New(pool, nil),
		audit:      pool,
		mint: func(workspaceID, _ string, _ []string, _ time.Duration) (string, error) {
			return "tok-" + workspaceID, nil
		},
	})
	get := func(path string) string {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body.String())
		}
		if !strings.HasPrefix(path, "/v1/admin/workspaces") {
			return w.Body.String()
		}
		// The roster comes from a map, so its order differs call to call; compare it as a sorted list.
		var rows []json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		sort.Slice(rows, func(i, j int) bool { return string(rows[i]) < string(rows[j]) })
		out, _ := json.Marshal(rows)
		return string(out)
	}
	adminReads := []string{
		"/v1/admin/usage/summary",
		"/v1/admin/usage/summary?window=720h",
		"/v1/admin/workspaces",
		"/v1/admin/routing-decisions/summary",
		"/v1/admin/distill/attribution",
		"/v1/admin/distill/attribution?view=pairs",
	}
	before := map[string]string{}
	for _, p := range adminReads {
		before[p] = get(p)
	}
	if !strings.Contains(before["/v1/admin/usage/summary"], `"requests":3`) {
		t.Fatalf("the real totals are not what was asked: %s", before["/v1/admin/usage/summary"])
	}

	// 100 synthetic workspaces, through the real route; each asks a question, half are served from
	// their shared pool, and two of them share a conversion.
	req := httptest.NewRequest(http.MethodPost, "/v1/synthetic/workspaces", strings.NewReader(`{"count":100}`))
	req.Header.Set(syntheticKeyHeader, "the-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var created struct {
		Workspaces []syntheticUser `json:"workspaces"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || len(created.Workspaces) != 100 {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	for i, u := range created.Workspaces {
		ask(u.WorkspaceID, "upstream", 0.002)
		if i%2 == 0 {
			ask(u.WorkspaceID, "cache_hit_pooled", 0)
		}
	}
	shareConversion(created.Workspaces[0].WorkspaceID, created.Workspaces[1].WorkspaceID)

	for _, p := range adminReads {
		if after := get(p); after != before[p] {
			t.Errorf("GET %s moved when synthetic workspaces asked questions:\nbefore %s\nafter  %s", p, before[p], after)
		}
	}

	// The harness's own view shows exactly its traffic.
	var usage struct {
		Summary opsusage.Summary `json:"summary"`
	}
	if err := json.Unmarshal([]byte(get("/v1/admin/usage/summary?synthetic=only")), &usage); err != nil {
		t.Fatal(err)
	}
	s := usage.Summary
	if s.Audience != "synthetic" || s.Workspaces != 100 || s.WithTraffic != 100 || s.Requests != 150 ||
		s.ServesBySource["upstream"] != 100 || s.ServesBySource["cache_hit_pooled"] != 50 || s.InputTokens != 150*120 {
		t.Errorf("synthetic usage = %+v, want 100 workspaces, 150 requests (100 upstream, 50 pooled)", s)
	}
	var roster []workspace.Workspace
	if err := json.Unmarshal([]byte(get("/v1/admin/workspaces?synthetic=only")), &roster); err != nil || len(roster) != 100 {
		t.Errorf("synthetic roster has %d workspaces (%v), want 100", len(roster), err)
	}
	if got := get("/v1/admin/routing-decisions/summary?synthetic=only"); !strings.Contains(got, `"total_requests":150`) {
		t.Errorf("synthetic routing summary = %s, want 150 requests", got)
	}
	if got := get("/v1/admin/distill/attribution?view=pairs&synthetic=only"); !strings.Contains(got, created.Workspaces[0].WorkspaceID) {
		t.Errorf("synthetic distill pairs = %s, want the pair the harness made", got)
	}

	// A typo is refused, never read as "everything".
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/usage/summary?synthetic=yes", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("?synthetic=yes = %d, want 400", w.Code)
	}
}
