package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B19.6 — the month-end forecast of a seeded month matches a hand calculation, and the owner pauses and
// resumes an agent through the routes.
func TestAgentRoutes_TheMonthEndForecastAndPausingAnAgent(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-agents"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 100000000, 100000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	proxyKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k", Scopes: []string{auth.ScopeProxy}}
	call := func(who *auth.AuthContext, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	base := "/v1/workspaces/" + ws + "/agents"
	newAgent := func(workspace, name string) string {
		a, err := store.CreateAgent(ctx, workspace, name, "user-owner")
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	researcher, writer := newAgent(ws, "researcher"), newAgent(ws, "writer")
	outsider := newAgent("ws-other", "outsider")
	seed := func(workspace, at, kind string, legs map[string]int64) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		entry := uuid.New()
		for account, amount := range legs {
			if _, err := tx.Exec(ctx, `INSERT INTO agent_postings (entry_id, workspace_id, account, amount_ulxc, kind, ref, created_at)
				VALUES ($1, $2, $3, $4, $5, '', $6)`, entry, workspace, account, amount, kind, at); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	R, W := "agent:"+researcher, "agent:"+writer

	// April 2026, as of the 11th at midnight: ten of its thirty days gone, so the month ends at three times
	// its spend so far. The researcher has spent 3 LXC and paid the writer 6 (9 LXC); the writer has spent
	// 1.5 LXC and been paid 6, which is not its spend. Funding is not spend, and nothing before April or
	// after the 11th counts.
	seed(ws, "2026-03-31T23:00:00Z", "spend", map[string]int64{R: -2_000_000, "spend": 2_000_000})
	seed(ws, "2026-04-01T08:00:00Z", "fund", map[string]int64{"workspace": -30_000_000, R: 30_000_000})
	seed(ws, "2026-04-02T10:00:00Z", "spend", map[string]int64{R: -3_000_000, "spend": 3_000_000})
	seed(ws, "2026-04-05T10:00:00Z", "spend", map[string]int64{W: -1_500_000, "spend": 1_500_000})
	seed(ws, "2026-04-08T10:00:00Z", "pay", map[string]int64{R: -6_000_000, W: 6_000_000})
	seed(ws, "2026-04-20T10:00:00Z", "spend", map[string]int64{R: -10_000_000, "spend": 10_000_000})
	seed("ws-other", "2026-04-03T10:00:00Z", "spend", map[string]int64{"agent:" + outsider: -50_000_000, "spend": 50_000_000})

	code, body := call(proxyKey, http.MethodGet, base+"/forecast?at=2026-04-11T00:00:00Z", "")
	if code != http.StatusOK {
		t.Fatalf("forecast = %d %s", code, body)
	}
	var f economy.SpendForecast
	if err := json.Unmarshal([]byte(body), &f); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]int64{researcher: {9_000_000, 27_000_000}, writer: {1_500_000, 4_500_000}}
	if f.SpentULXC != 10_500_000 || f.ForecastULXC != 31_500_000 || len(f.Agents) != 2 {
		t.Errorf("the workspace spent %d and is forecast %d over %d agents, want 10,500,000 and 31,500,000 over 2", f.SpentULXC, f.ForecastULXC, len(f.Agents))
	}
	for _, a := range f.Agents {
		if w := want[a.AgentID]; a.SpentULXC != w[0] || a.ForecastULXC != w[1] {
			t.Errorf("agent %s spent %d, forecast %d; want %d and %d", a.Name, a.SpentULXC, a.ForecastULXC, w[0], w[1])
		}
	}
	if f.MonthStart.Format("2006-01-02") != "2026-04-01" || f.MonthEnd.Format("2006-01-02") != "2026-05-01" {
		t.Errorf("the month is %s – %s, want April", f.MonthStart, f.MonthEnd)
	}

	// Paused by its owner, an agent cannot pay; resumed, it can. A proxy key can do neither.
	if code, _ := call(proxyKey, http.MethodPost, base+"/"+researcher+"/pause", `{}`); code != http.StatusForbidden {
		t.Fatalf("a proxy key paused an agent: %d, want 403", code)
	}
	if code, body := call(owner, http.MethodPost, base+"/"+researcher+"/pause", `{"reason":"quarter-end review"}`); code != http.StatusOK {
		t.Fatalf("pause = %d %s", code, body)
	}
	pay := `{"to_agent_id":"` + writer + `","amount_ulxc":1000}`
	if code, body := call(owner, http.MethodPost, base+"/"+researcher+"/pay", pay); code != http.StatusForbidden || !strings.Contains(body, "quarter-end review") {
		t.Errorf("a paused agent's payment = %d %s, want 403 giving the reason", code, body)
	}
	_, book := call(owner, http.MethodGet, base, "")
	if !strings.Contains(book, `"paused_reason":"quarter-end review"`) {
		t.Errorf("the book does not show the agent paused: %s", book)
	}
	if code, body := call(owner, http.MethodPost, base+"/"+researcher+"/resume", ``); code != http.StatusOK {
		t.Fatalf("resume = %d %s", code, body)
	}
	if code, body := call(owner, http.MethodPost, base+"/"+researcher+"/pay", pay); code != http.StatusOK {
		t.Errorf("the resumed agent's payment = %d %s, want 200", code, body)
	}
	if code, body := call(owner, http.MethodGet, base+"/alerts", ""); code != http.StatusOK || !strings.Contains(body, `"rule":"An alert is raised`) {
		t.Errorf("alerts = %d %s, want the list and the rule", code, body)
	}
}
