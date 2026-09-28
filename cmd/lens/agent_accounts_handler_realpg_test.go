package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/migrations"
)

// B19.1 — a workspace creates two agents, gives each a key, funds them and takes funds back, through
// the real routes on a migrated schema.

func agentRoutesDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG agent routes test")
	}
	const schema = "cmd_agent_routes_realpg"
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("reset schema: %v", err)
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = conn.Close(ctx)
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestAgentRoutes_AWorkspaceFundsTwoAgentsAndTakesFundsBack(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-agents"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))

	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	proxyKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k", Scopes: []string{auth.ScopeProxy}}
	call := func(who *auth.AuthContext, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	base := "/v1/workspaces/" + ws + "/agents"

	if code, _ := call(proxyKey, http.MethodPost, base, `{"name":"researcher"}`); code != http.StatusForbidden {
		t.Fatalf("a proxy key created an agent: %d, want 403", code)
	}
	code, a := call(owner, http.MethodPost, base, `{"name":"researcher"}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, a)
	}
	researcher := a["id"].(string)
	_, b := call(owner, http.MethodPost, base, `{"name":"writer"}`)
	writer := b["id"].(string)

	code, k := call(owner, http.MethodPost, base+"/"+researcher+"/keys", `{"name":"researcher key"}`)
	if code != http.StatusCreated || !strings.HasPrefix(k["key"].(string), tenant.KeyPrefix) {
		t.Fatalf("issue key = %d %v", code, k)
	}

	fund := func(agent string, ulxc string) (int, map[string]any) {
		return call(owner, http.MethodPost, base+"/"+agent+"/fund", `{"amount_ulxc":`+ulxc+`}`)
	}
	if code, out := fund(researcher, "10000000"); code != http.StatusOK || out["balance_ulxc"].(float64) != 10_000_000 {
		t.Fatalf("fund researcher = %d %v", code, out)
	}
	if code, out := fund(writer, "5000000"); code != http.StatusOK || out["balance_ulxc"].(float64) != 5_000_000 {
		t.Fatalf("fund writer = %d %v", code, out)
	}
	// 20 LXC in the workspace, 15 held by agents: 6 more cannot be allocated.
	if code, _ := fund(writer, "6000000"); code != http.StatusConflict {
		t.Fatalf("funding beyond the unallocated 5 LXC = %d, want 409", code)
	}
	if code, _ := call(proxyKey, http.MethodPost, base+"/"+writer+"/withdraw", `{"amount_ulxc":1}`); code != http.StatusForbidden {
		t.Fatalf("a proxy key withdrew from an agent: %d, want 403", code)
	}
	if code, out := call(owner, http.MethodPost, base+"/"+researcher+"/withdraw", `{"amount_ulxc":2000000}`); code != http.StatusOK || out["balance_ulxc"].(float64) != 8_000_000 {
		t.Fatalf("withdraw = %d %v", code, out)
	}
	if code, _ := call(owner, http.MethodPost, base+"/"+writer+"/withdraw", `{"amount_ulxc":5000001}`); code != http.StatusConflict {
		t.Fatalf("withdrawing more than the writer holds = %d, want 409", code)
	}

	code, book := call(proxyKey, http.MethodGet, base, ``)
	if code != http.StatusOK {
		t.Fatalf("book = %d", code)
	}
	if book["workspace_balance_ulxc"].(float64) != 20_000_000 || book["allocated_ulxc"].(float64) != 13_000_000 ||
		book["unallocated_ulxc"].(float64) != 7_000_000 || len(book["agents"].([]any)) != 2 {
		t.Errorf("book = %v, want workspace 20,000,000 = allocated 13,000,000 + unallocated 7,000,000, two agents", book)
	}
	var sum, n int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint, count(*) FROM agent_postings`).Scan(&sum, &n); err != nil {
		t.Fatal(err)
	}
	if sum != 0 || n != 6 {
		t.Errorf("agent_postings: %d rows summing to %d, want 6 rows (three entries) summing to 0", n, sum)
	}
}

// B19.2 — the owner sets an agent's rules and decides its approvals through the routes; a proxy key
// can read the rules but not change them, and a misspelt or invalid rule is refused rather than
// stored as no rule.
func TestAgentRoutes_TheOwnerSetsAnAgentsRulesAndDecidesItsApprovals(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-agents"
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
	agent, err := store.CreateAgent(ctx, ws, "researcher")
	if err != nil {
		t.Fatal(err)
	}
	rules := "/v1/workspaces/" + ws + "/agents/" + agent.ID + "/rules"
	const set = `{"max_per_request_ulxc":2000000,"daily_limit_ulxc":10000000,"approval_above_ulxc":1000000,
		"allowed_models":["gpt-4o"],"allowed_providers":["openai"],"active_from":"09:00","active_until":"18:00","timezone":"Europe/Bucharest"}`

	if code, _ := call(proxyKey, http.MethodPut, rules, set); code != http.StatusForbidden {
		t.Errorf("a proxy key set an agent's rules: %d, want 403", code)
	}
	for _, bad := range []string{`{"daily_limit":5}`, `{"active_from":"09:00"}`, `{"timezone":"Mars/Olympus"}`, `{"daily_limit_ulxc":-1}`} {
		if code, body := call(owner, http.MethodPut, rules, bad); code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d %s, want 400", bad, code, body)
		}
	}
	if code, body := call(owner, http.MethodPut, rules, set); code != http.StatusOK {
		t.Fatalf("PUT rules = %d %s", code, body)
	}
	code, body := call(proxyKey, http.MethodGet, rules, "")
	var got economy.AgentRules
	if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil ||
		got.DailyLimitULXC != 10_000_000 || got.Timezone != "Europe/Bucharest" || len(got.AllowedModels) != 1 {
		t.Errorf("GET rules = %d %s", code, body)
	}
	if code, _ := call(owner, http.MethodPut, "/v1/workspaces/"+ws+"/agents/agt_nobody/rules", set); code != http.StatusNotFound {
		t.Errorf("rules for an agent that is not this workspace's = %d, want 404", code)
	}

	// An approval the agent's rules filed: listed, approved once by the owner, never by a proxy key.
	if _, err := pool.Exec(ctx, `INSERT INTO agent_approvals (id, workspace_id, agent_id, fingerprint, amount_ulxc, model)
		VALUES ('apr_1', $1, $2, 'fp', 3000000, 'gpt-4o')`, ws, agent.ID); err != nil {
		t.Fatal(err)
	}
	approvals := "/v1/workspaces/" + ws + "/agents/approvals"
	if code, body := call(owner, http.MethodGet, approvals, ""); code != http.StatusOK || !strings.Contains(body, `"status":"pending"`) {
		t.Errorf("GET approvals = %d %s", code, body)
	}
	if code, _ := call(proxyKey, http.MethodPost, approvals+"/apr_1/approve", ""); code != http.StatusForbidden {
		t.Errorf("a proxy key approved its own request: %d, want 403", code)
	}
	if code, body := call(owner, http.MethodPost, approvals+"/apr_1/approve", ""); code != http.StatusOK || !strings.Contains(body, `"status":"approved"`) {
		t.Errorf("approve = %d %s", code, body)
	}
	if code, _ := call(owner, http.MethodPost, approvals+"/apr_1/deny", ""); code != http.StatusNotFound {
		t.Errorf("deciding an approval twice = %d, want 404", code)
	}
}
