package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/tenant"
)

// B19.17 — a weekly schedule to a marketplace listing bills the payer's marketplace bill once a week, across
// a restart, and a tick the agent's rules refuse is recorded refused and billed nothing.
func TestAgentSchedule_AWeeklyPaymentToAListingIsBilledOnceAWeek(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, buyer = "ws-seller", "ws-buyer"
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetListingCharger(store)
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	mountAgentAccountRoutes(r, bank, tenant.NewStore(pool))
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	code, body := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings",
		`{"kind":"prompt","title":"Weekly digest","price_per_use_ulxc":50000,"artifact":{"template":"Digest {{week}}"}}`)
	var listing market.Listing
	if err := json.Unmarshal([]byte(body), &listing); err != nil || code != http.StatusCreated {
		t.Fatalf("publish = %d %s", code, body)
	}
	agent, err := bank.CreateAgent(ctx, buyer, "reporter", "owner-"+buyer)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now().UTC().Add(-15 * 24 * time.Hour).Truncate(time.Second)
	code, body = call(buyer, http.MethodPost, "/v1/workspaces/"+buyer+"/agents/"+agent.ID+"/schedules",
		`{"to_listing_id":"`+listing.ID+`","every":"week","memo":"the weekly digest","first_run_at":"`+start.Format(time.RFC3339)+`"}`)
	var sc economy.AgentSchedule
	if err := json.Unmarshal([]byte(body), &sc); err != nil || code != http.StatusCreated || sc.ToListingID != listing.ID || sc.AmountULXC != 50_000 {
		t.Fatalf("a weekly schedule to the listing = %d %s", code, body)
	}
	// A schedule cannot pay the workspace's own listing.
	own, err := bank.CreateAgent(ctx, seller, "self", "owner-"+seller)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/agents/"+own.ID+"/schedules",
		`{"to_listing_id":"`+listing.ID+`","every":"week"}`); code != http.StatusBadRequest || !strings.Contains(body, "own listing") {
		t.Errorf("a schedule to the workspace's own listing = %d %s, want 400", code, body)
	}

	uses := func() []string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT id || ' ' || price_ulxc || ' ' || charge || ' ' || (used_at = ran_at)::text || ' ' || extract(epoch FROM used_at)::bigint
			FROM market_uses WHERE agent_id = $1 ORDER BY used_at`, agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}

	// The first tick pays; running again at the same moment pays nothing more.
	for range 2 {
		if _, err := bank.RunAgentSchedules(ctx, start); err != nil {
			t.Fatal(err)
		}
	}
	if got := uses(); len(got) != 1 || !strings.HasSuffix(got[0], " 50000 billed true "+strconv.FormatInt(start.Unix(), 10)) {
		t.Fatalf("after the first tick, the agent's uses = %q, want one billed 50,000 µLXC use stamped at the tick", got)
	}
	// Lens restarts; a week later the second tick pays, once.
	restarted := economy.NewDualTokenStore(nil, pool, nil)
	restarted.SetListingCharger(market.NewStore(pool))
	week2 := start.Add(7*24*time.Hour + time.Minute)
	for range 2 {
		if _, err := restarted.RunAgentSchedules(ctx, week2); err != nil {
			t.Fatal(err)
		}
	}
	paid := uses()
	if len(paid) != 2 || !strings.HasSuffix(paid[1], " "+strconv.FormatInt(start.Add(7*24*time.Hour).Unix(), 10)) {
		t.Fatalf("after the restart and a week, the agent's uses = %q, want a second one stamped a week after the first", paid)
	}

	// The marketplace bill gets each tick once, however many passes run.
	stripeFake := &marketStripe{}
	svc := billing.New(pool, bank, stripeFake, "whsec_test").WithMarketBill(stripeFake, "price_market", "talyvor_marketplace_use", store)
	for range 2 {
		if _, err := store.MeterPending(ctx, svc, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if len(stripeFake.events) != 2 || stripeFake.events[0].customer != "cus_"+buyer || stripeFake.events[0].value != 50_000 ||
		stripeFake.events[0].identifier != strings.Fields(paid[0])[0] || stripeFake.events[1].identifier != strings.Fields(paid[1])[0] {
		t.Fatalf("the buyer's marketplace bill got %+v, want the two ticks once each", stripeFake.events)
	}

	// Its rules now allow at most 0.01 LXC a payment: the third tick is refused, recorded, and billed nothing.
	if _, err := bank.SetAgentRules(ctx, buyer, agent.ID, economy.AgentRules{MaxPerRequestULXC: 10_000}); err != nil {
		t.Fatal(err)
	}
	res, err := bank.RunAgentSchedules(ctx, start.Add(14*24*time.Hour+time.Minute))
	if err != nil || res.Refused != 1 || res.Paid != 0 {
		t.Fatalf("the third tick = %+v (%v), want refused", res, err)
	}
	_, body = call(buyer, http.MethodGet, "/v1/workspaces/"+buyer+"/agents/schedules/"+sc.ID+"/runs", "")
	var runs struct {
		Runs []economy.AgentScheduleRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(body), &runs); err != nil || len(runs.Runs) != 3 ||
		runs.Runs[0].Outcome != "refused" || !strings.Contains(runs.Runs[0].Detail, "limit per request") || runs.Runs[0].UseID != "" ||
		runs.Runs[1].Outcome != "paid" || runs.Runs[1].UseID != strings.Fields(paid[1])[0] || runs.Runs[2].UseID != strings.Fields(paid[0])[0] {
		t.Errorf("the schedule's runs = %s, want refused, then the two paid ticks naming their uses", body)
	}
	if got := uses(); len(got) != 2 {
		t.Errorf("the refused tick recorded a use: %q", got)
	}
}
