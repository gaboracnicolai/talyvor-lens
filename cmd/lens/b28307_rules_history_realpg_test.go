package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.307 — every change to an agent's rules is a version, and rolling back restores the earlier agent_rules exactly
// and records who changed it: asked through the routes the console uses, by three people of one workspace.
func TestB28307_RollingBackRestoresTheEarlierAgentRulesExactlyAndRecordsWhoChangedIt(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-rules-history"
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	call := func(user, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: user, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	a, err := store.CreateAgent(ctx, ws, "researcher", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + ws + "/agents/" + a.ID
	// row is the agent's rules exactly as stored: every column but updated_at.
	row := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT (to_jsonb(r) - 'updated_at')::text FROM agent_rules r WHERE agent_id = $1`, a.ID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	history := func() []economy.AgentRulesVersion {
		t.Helper()
		code, body := call("alice", http.MethodGet, base+"/rules/history", "")
		if code != http.StatusOK {
			t.Fatalf("GET rules/history = %d %s", code, body)
		}
		var out struct{ Versions []economy.AgentRulesVersion }
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return out.Versions
	}

	if code, body := call("alice", http.MethodPut, base+"/rules", `{"daily_limit_ulxc":2000000,"hourly_limit_ulxc":500000,
		"allowed_models":["gpt-4o"],"model_daily_limits_ulxc":{"gpt-4o":1000000},"blocked_payees":["agt_nobody"],
		"requests_per_minute":30,"active_from":"09:00","active_until":"17:00","timezone":"Europe/Bucharest"}`); code != http.StatusOK {
		t.Fatalf("alice's PUT rules = %d %s", code, body)
	}
	first := row()
	if code, body := call("bob", http.MethodPost, base+"/rules/template", `{"template":"researcher"}`); code != http.StatusOK {
		t.Fatalf("bob's template = %d %s", code, body)
	}
	if row() == first {
		t.Fatal("the template left the rules as they were; the test needs it to change them")
	}
	// Applying the same template again changes nothing, so it is no new version.
	if code, body := call("bob", http.MethodPost, base+"/rules/template", `{"template":"researcher"}`); code != http.StatusOK {
		t.Fatalf("bob's second template = %d %s", code, body)
	}
	got := history()
	if len(got) != 2 || got[0].Version != 2 || got[0].ChangedBy != "jwt:user:bob" || got[0].Change != "template researcher" ||
		got[1].Version != 1 || got[1].ChangedBy != "jwt:user:alice" || got[1].Change != "set" || got[1].Rules.DailyLimitULXC != 2_000_000 {
		t.Fatalf("history after alice's save and bob's template = %+v, want version 2 by bob, then 1 by alice", got)
	}

	code, body := call("carol", http.MethodPost, base+"/rules/rollback", `{"version":1}`)
	if code != http.StatusOK {
		t.Fatalf("carol's rollback to version 1 = %d %s", code, body)
	}
	var rules economy.AgentRules
	if err := json.Unmarshal([]byte(body), &rules); err != nil || rules.DailyLimitULXC != 2_000_000 || rules.Timezone != "Europe/Bucharest" {
		t.Fatalf("the rollback answered %s, want version 1's rules", body)
	}
	if now := row(); now != first {
		t.Fatalf("after rolling back to version 1 agent_rules is\n%s\nwant exactly\n%s", now, first)
	}
	got = history()
	if len(got) != 3 || got[0].Version != 3 || got[0].ChangedBy != "jwt:user:carol" || got[0].Change != "rollback to 1" {
		t.Fatalf("history after carol's rollback = %+v, want version 3, rollback to 1 by carol", got)
	}
	if v3, v1 := mustJSON(t, got[0].Rules), mustJSON(t, got[2].Rules); v3 != v1 {
		t.Errorf("version 3's rules are %s, want version 1's %s", v3, v1)
	}

	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, base + "/rules/rollback", `{"version":99}`, http.StatusNotFound},
		{http.MethodPost, base + "/rules/rollback", `{}`, http.StatusBadRequest},
		{http.MethodGet, "/v1/workspaces/" + ws + "/agents/agt_nobody/rules/history", "", http.StatusNotFound},
		{http.MethodPost, "/v1/workspaces/" + ws + "/agents/agt_nobody/rules/rollback", `{"version":1}`, http.StatusNotFound},
	} {
		if code, got := call("carol", c.method, c.path, c.body); code != c.want {
			t.Errorf("%s %s %s = %d %s, want %d", c.method, c.path, c.body, code, got, c.want)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
