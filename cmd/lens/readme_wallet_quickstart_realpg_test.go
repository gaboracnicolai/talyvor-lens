package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.11 — the README opens on an agent wallet quickstart. This replays its curl calls, as written in the
// README, through the real agent routes on a migrated schema with the admin key the README uses, and
// checks that the agent ended up funded on its ledger, ruled, and holding a key of its own. The model
// call (step 5) goes through the proxy, which this harness does not mount; B19.2's tests enforce the
// rules on that path.

var (
	quickstartMethod = regexp.MustCompile(`-X ([A-Z]+)`)
	quickstartURL    = regexp.MustCompile(`\$LENS(/\S+)`)
	quickstartBody   = regexp.MustCompile(`-d '([^']*)'`)
	quickstartAssign = regexp.MustCompile(`^(\w+)=\$\(curl .*\| jq -r \.(\w+)\)$`)
)

func TestREADMEWalletQuickstartRuns(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "default"
	// docs/quickstart.md step 4: the workspace's first 50 LXC.
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 50000000, 50000000)`, ws); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, economy.NewDualTokenStore(nil, pool, nil), tenant.NewStore(pool))
	admin := &auth.AuthContext{Scopes: []string{auth.ScopeProxy, auth.ScopeAnalytics, auth.ScopeAdmin, auth.ScopeKeys},
		AuthMethod: auth.MethodGlobalKey, IsAdmin: true}

	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	section := string(readme)
	i := strings.Index(section, "\n## Agent wallet quickstart\n")
	if i < 0 {
		t.Fatal("the README has no \"## Agent wallet quickstart\" section")
	}
	section = section[i+1:]
	if j := strings.Index(section[3:], "\n## "); j >= 0 {
		section = section[:j+3]
	}
	start := strings.Index(section, "```bash\n")
	end := strings.Index(section[start+8:], "```")
	if start < 0 || end < 0 {
		t.Fatal("the quickstart has no ```bash block")
	}
	script := strings.ReplaceAll(section[start+8:start+8+end], "\\\n", " ")

	vars := map[string]string{"WS": ws}
	replayed := 0
	for _, line := range strings.Split(script, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if !strings.Contains(line, "curl ") {
			continue
		}
		u := quickstartURL.FindStringSubmatch(line)
		if u == nil {
			t.Fatalf("a quickstart curl names no $LENS URL: %s", line)
		}
		path := os.Expand(u[1], func(k string) string { return vars[k] })
		if strings.HasPrefix(path, "/v1/proxy/") {
			continue
		}
		method := http.MethodGet
		if m := quickstartMethod.FindStringSubmatch(line); m != nil {
			method = m[1]
		}
		body := ""
		if b := quickstartBody.FindStringSubmatch(line); b != nil {
			body = b[1]
		}
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), admin))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code/100 != 2 {
			t.Fatalf("%s %s %s = %d %s", method, path, body, w.Code, w.Body.String())
		}
		if a := quickstartAssign.FindStringSubmatch(line); a != nil {
			var out map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			v, _ := out[a[2]].(string)
			if v == "" {
				t.Fatalf("%s %s has no %q to put in $%s: %s", method, path, a[2], a[1], w.Body.String())
			}
			vars[a[1]] = v
		}
		replayed++
	}
	if replayed < 6 {
		t.Fatalf("replayed %d quickstart calls, want the 6 outside the proxy", replayed)
	}

	agent := vars["AGENT"]
	var funded int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0) FROM agent_postings
		WHERE workspace_id = $1 AND account = 'agent:' || $2 AND kind = 'fund'`, ws, agent).Scan(&funded); err != nil {
		t.Fatal(err)
	}
	if funded != 20_000_000 {
		t.Fatalf("the agent's ledger holds %d µLXC of funding, want the quickstart's 20000000 ($2)", funded)
	}
	rules, err := economy.NewDualTokenStore(nil, pool, nil).GetAgentRules(ctx, ws, agent)
	if err != nil {
		t.Fatal(err)
	}
	if rules.DailyLimitULXC != 5_000_000 || rules.ApprovalAboveULXC != 1_000_000 || len(rules.AllowedModels) != 1 || rules.AllowedModels[0] != "gpt-4o-mini" {
		t.Fatalf("the agent's rules are %+v, not the quickstart's", rules)
	}
	if !strings.HasPrefix(vars["AGENT_KEY"], tenant.KeyPrefix) {
		t.Fatalf("the agent's key is %q, want a %s key", vars["AGENT_KEY"], tenant.KeyPrefix)
	}
}
