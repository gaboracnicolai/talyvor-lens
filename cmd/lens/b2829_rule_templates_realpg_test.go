package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.29 — rule templates, through the routes the console uses: an agent created from a template holds exactly
// its rules, and applying another to an agent with rules of every kind leaves it holding exactly that template's,
// nothing kept from before. An unknown template creates nothing and changes nothing.
func TestB2829_ApplyingATemplateWritesAgentRulesEqualToTheTemplate(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-rule-templates"
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	decode := func(body string, into any) {
		t.Helper()
		if err := json.Unmarshal([]byte(body), into); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
	}
	agents := "/v1/workspaces/" + ws + "/agents"

	code, body := call(http.MethodGet, agents+"/rule-templates", "")
	var catalog struct {
		Templates []struct {
			ID, Name, Summary string
			Rules             map[string]any
		}
	}
	decode(body, &catalog)
	templates := map[string]map[string]any{}
	for _, tm := range catalog.Templates {
		templates[tm.ID] = tm.Rules
	}
	if code != http.StatusOK || len(templates) != 3 || templates["support-bot"] == nil || templates["researcher"] == nil || templates["coder"] == nil {
		t.Fatalf("GET rule-templates = %d %s, want support-bot, researcher and coder", code, body)
	}
	var every map[string]any // every rule Lens holds, as an agent with none set reads them
	_, body = call(http.MethodGet, agents+"/"+mustCreateAgent(t, store, ws)+"/rules", "")
	decode(body, &every)
	for id, rules := range templates {
		for name := range every {
			if v, ok := rules[name]; !ok || v == nil {
				t.Errorf("template %s does not name the rule %s, so applying it would keep the agent's", id, name)
			}
		}
	}
	rulesOf := func(agentID string) map[string]any {
		t.Helper()
		code, body := call(http.MethodGet, agents+"/"+agentID+"/rules", "")
		if code != http.StatusOK {
			t.Fatalf("GET rules = %d %s", code, body)
		}
		var got map[string]any
		decode(body, &got)
		return got
	}

	// Created from Support bot, the agent holds exactly Support bot's rules.
	code, body = call(http.MethodPost, agents, `{"name":"helpdesk","template":"support-bot"}`)
	var created struct {
		ID       string
		Template string
	}
	decode(body, &created)
	if code != http.StatusCreated || created.ID == "" || created.Template != "support-bot" {
		t.Fatalf("POST agents from support-bot = %d %s", code, body)
	}
	if got := rulesOf(created.ID); !reflect.DeepEqual(got, templates["support-bot"]) {
		t.Errorf("an agent created from support-bot holds %v, want exactly %v", got, templates["support-bot"])
	}

	// Rules of every kind, none of them Researcher's; applying Researcher leaves exactly Researcher's.
	if code, body := call(http.MethodPut, agents+"/"+created.ID+"/rules", `{"max_per_request_ulxc":7,"hourly_limit_ulxc":8,
		"daily_limit_ulxc":9,"weekly_limit_ulxc":10,"monthly_limit_ulxc":11,"approval_above_ulxc":12,"allowed_models":["gpt-4o"],
		"allowed_providers":["openai"],"allowed_listings":["lst_x"],"active_from":"09:00","active_until":"17:00","timezone":"Europe/Berlin",
		"pause_on_unusual_spend":true,"model_daily_limits_ulxc":{"gpt-4o":13},"requests_per_minute":14,"allowed_payees":["agt_a"],
		"blocked_payees":["agt_b"],"payee_daily_limits_ulxc":{"agt_a":15}}`); code != http.StatusOK {
		t.Fatalf("PUT rules = %d %s", code, body)
	}
	if code, body := call(http.MethodPost, agents+"/"+created.ID+"/rules/template", `{"template":"researcher"}`); code != http.StatusOK {
		t.Fatalf("apply researcher = %d %s", code, body)
	}
	if got := rulesOf(created.ID); !reflect.DeepEqual(got, templates["researcher"]) {
		t.Errorf("after applying researcher the agent holds %v, want exactly %v", got, templates["researcher"])
	}

	// An unknown template creates no agent and changes no rules; an unknown agent is 404.
	var before, after int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM agent_accounts WHERE workspace_id = $1`, ws).Scan(&before)
	if code, body := call(http.MethodPost, agents, `{"name":"nobody","template":"poet"}`); code != http.StatusBadRequest || !strings.Contains(body, "coder") {
		t.Errorf("POST agents from an unknown template = %d %s, want 400 naming the templates", code, body)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM agent_accounts WHERE workspace_id = $1`, ws).Scan(&after)
	if after != before {
		t.Errorf("an unknown template created an agent: %d agents, then %d", before, after)
	}
	if code, body := call(http.MethodPost, agents+"/"+created.ID+"/rules/template", `{"template":"poet"}`); code != http.StatusBadRequest {
		t.Errorf("apply an unknown template = %d %s, want 400", code, body)
	}
	if got := rulesOf(created.ID); !reflect.DeepEqual(got, templates["researcher"]) {
		t.Errorf("an unknown template changed the rules to %v", got)
	}
	if code, body := call(http.MethodPost, agents+"/agt_missing/rules/template", `{"template":"coder"}`); code != http.StatusNotFound {
		t.Errorf("apply to an unknown agent = %d %s, want 404", code, body)
	}
}

func mustCreateAgent(t *testing.T, store *economy.DualTokenStore, ws string) string {
	t.Helper()
	a, err := store.CreateAgent(context.Background(), ws, "blank", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}
