package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B17.118 — an agent serves 60 requests, each held and settled on its account, and its statement, read with no
// limit the way Agent Wallets reads it, shows all 60 holds: three lines a request no longer push half of them off.
func TestB17118_AStatementReadUnaskedShowsEveryHoldOfSixtyRequests(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, key = "ws-statement-minute", "key-statement-minute"
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreditLXC(ctx, ws, 100_000_000, "top-up", nil); err != nil {
		t.Fatal(err)
	}
	a, err := store.CreateAgent(ctx, ws, "rate", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, a.ID, 50_000_000); err != nil {
		t.Fatal(err)
	}
	const served = 60
	for i := range served {
		ref := fmt.Sprintf("res-%02d", i)
		req := economy.WithAgentRequest(ctx, economy.AgentRequest{Model: "gpt-4o", Provider: "openai"})
		if err := store.ReserveLXCForAgent(req, key, ws, ref, 20_000, economy.AgentDebitMeta{}); err != nil {
			t.Fatalf("hold %d: %v", i, err)
		}
		if _, _, err := store.SettleLXCReservation(ctx, ref, 12_000, economy.AgentDebitMeta{}); err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/agents/"+a.ID+"/statement", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(),
		&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var st struct {
		Lines []economy.AgentStatementLine `json:"lines"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); w.Code != http.StatusOK || err != nil {
		t.Fatalf("statement = %d %s", w.Code, w.Body.String())
	}
	var on int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE account = $1`, "agent:"+a.ID).Scan(&on); err != nil {
		t.Fatal(err)
	}
	holds := 0
	for _, l := range st.Lines {
		if l.Kind == "hold" {
			holds++
		}
	}
	if holds != served || len(st.Lines) != on {
		t.Fatalf("the statement shows %d holds in %d lines; the account has %d lines from %d requests, want every one", holds, len(st.Lines), on, served)
	}
}
