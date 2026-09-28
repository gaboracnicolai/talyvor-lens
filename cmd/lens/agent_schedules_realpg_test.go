package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B19.8 — a weekly payment runs once a week across a restart and two runners, a top-up fires once when
// the balance crosses its threshold, and every run is a ledger entry.
func TestAgentRoutes_AWeeklyPaymentRunsOnceAWeekAndATopUpFiresOnce(t *testing.T) {
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
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	base := "/v1/workspaces/" + ws + "/agents"
	payer, err := store.CreateAgent(ctx, ws, "payer", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateAgent(ctx, ws, "payee", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, payer.ID, 10_000_000); err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	code, body := call(http.MethodPost, base+"/"+payer.ID+"/schedules",
		`{"to_agent_id":"`+payee.ID+`","amount_ulxc":1000000,"memo":"weekly retainer","every":"week","first_run_at":"`+t0.Format(time.RFC3339)+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create schedule = %d %s", code, body)
	}
	var sc economy.AgentSchedule
	if err := json.Unmarshal([]byte(body), &sc); err != nil {
		t.Fatal(err)
	}

	paid := 0
	run := func(s *economy.DualTokenStore, at time.Time) {
		t.Helper()
		res, err := s.RunAgentSchedules(ctx, at)
		if err != nil {
			t.Fatalf("run at %s: %v", at, err)
		}
		paid += res.Paid
	}
	expectPaid := func(when string, want int) {
		t.Helper()
		if paid != want {
			t.Fatalf("%s: %d payments made in all, want %d", when, paid, want)
		}
	}
	run(store, t0.Add(-time.Minute))
	expectPaid("a minute before the first tick", 0)
	run(store, t0)
	expectPaid("at the first tick", 1)
	run(store, t0.Add(24*time.Hour))
	expectPaid("a day later", 1)
	// A restart: a new store over the same database finds nothing due.
	restarted := economy.NewDualTokenStore(nil, pool, nil)
	run(restarted, t0.Add(24*time.Hour))
	expectPaid("a day later, after a restart", 1)
	// A week on, two Lens processes run the same tick at once: it is paid once.
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, s := range []*economy.DualTokenStore{store, restarted} {
		wg.Add(1)
		go func(s *economy.DualTokenStore) {
			defer wg.Done()
			res, err := s.RunAgentSchedules(ctx, t0.Add(7*24*time.Hour))
			if err != nil {
				t.Errorf("concurrent run: %v", err)
			}
			mu.Lock()
			paid += res.Paid
			mu.Unlock()
		}(s)
	}
	wg.Wait()
	expectPaid("a week on, run twice at once", 2)
	// Down for two weeks: each missed tick is paid once when Lens is back.
	run(restarted, t0.Add(21*24*time.Hour+time.Hour))
	expectPaid("three weeks on, after two weeks down", 4)

	// Every run is its payment in the ledger: four runs, four 'pay' entries of 1 LXC, the payee holds 4.
	code, body = call(http.MethodGet, base+"/schedules/"+sc.ID+"/runs", "")
	var runs struct {
		Runs []economy.AgentScheduleRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(body), &runs); code != http.StatusOK || err != nil || len(runs.Runs) != 4 {
		t.Fatalf("runs = %d %s, want four", code, body)
	}
	for i, run := range runs.Runs {
		wantTick := t0.AddDate(0, 0, 7*(3-i))
		var amount int64
		var memo string
		if err := pool.QueryRow(ctx, `SELECT amount_ulxc, ref FROM agent_postings WHERE entry_id = $1::uuid AND account = $2 AND kind = 'pay'`,
			run.EntryID, "agent:"+payee.ID).Scan(&amount, &memo); err != nil {
			t.Fatalf("run %+v has no payment in the ledger: %v", run, err)
		}
		if run.Outcome != "paid" || !run.TickAt.Equal(wantTick) || amount != 1_000_000 || memo != "weekly retainer" {
			t.Errorf("run %d = %+v paying %d %q, want the %s tick paying 1,000,000 'weekly retainer'", i, run, amount, memo, wantTick)
		}
	}
	var entries int64
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT entry_id) FROM agent_postings WHERE kind = 'pay'`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 4 {
		t.Errorf("%d payment entries in the ledger, want 4", entries)
	}

	// The top-up: below 5 LXC, back up to 8. The payer holds 6 after four payments: nothing to do.
	if code, body := call(http.MethodPut, base+"/"+payer.ID+"/topup", `{"below_ulxc":5000000,"to_ulxc":8000000}`); code != http.StatusOK {
		t.Fatalf("set top-up = %d %s", code, body)
	}
	later := t0.Add(21*24*time.Hour + 2*time.Hour)
	topUps := func() (n, sum int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings
			WHERE kind = 'topup' AND account = $1`, "agent:"+payer.ID).Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		return n, sum
	}
	run(store, later)
	if n, _ := topUps(); n != 0 {
		t.Fatalf("a top-up fired at 6 LXC with a threshold of 5")
	}
	// It pays 2 LXC and falls to 4: the next run brings it back to 8, once, and the run after does nothing.
	if code, body := call(http.MethodPost, base+"/"+payer.ID+"/pay", `{"to_agent_id":"`+payee.ID+`","amount_ulxc":2000000}`); code != http.StatusOK {
		t.Fatalf("pay = %d %s", code, body)
	}
	run(store, later)
	run(restarted, later)
	if n, sum := topUps(); n != 1 || sum != 4_000_000 {
		t.Errorf("%d top-ups of %d µLXC in all, want one of 4,000,000", n, sum)
	}
	book, err := store.AgentBook(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range book.Agents {
		if want := map[string]int64{payer.ID: 8_000_000, payee.ID: 6_000_000}[a.ID]; a.BalanceULXC != want {
			t.Errorf("%s holds %d, want %d", a.Name, a.BalanceULXC, want)
		}
	}
}
