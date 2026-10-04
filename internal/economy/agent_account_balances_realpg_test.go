package economy

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// B28.297 — an agent's balance is a stored running total (0185), not a re-sum of its postings.

// unreconciledSQL lists every account whose stored balance is not the sum of its postings, or that has
// postings and no stored balance, or a stored balance and no postings. The shared sides are not stored.
const unreconciledSQL = `SELECT COALESCE(p.workspace_id, b.workspace_id) || ' ' || COALESCE(p.account, b.account)
  FROM (SELECT workspace_id, account, sum(amount_ulxc)::bigint AS total FROM agent_postings
         WHERE account NOT IN ('workspace', 'spend', 'cashed_out') GROUP BY workspace_id, account) p
  FULL JOIN agent_account_balances b ON b.workspace_id = p.workspace_id AND b.account = p.account
 WHERE p.total IS DISTINCT FROM b.balance_ulxc`

func requireReconciled(t *testing.T, ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) {
	t.Helper()
	rows, err := db.Query(ctx, unreconciledSQL)
	if err != nil {
		t.Fatal(err)
	}
	off, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(off) > 0 {
		t.Fatalf("stored balances disagree with the postings for %v", off)
	}
}

// An agent funded 10,000 times, then withdrawn from, moved into a pot and spent through its key: its balance
// read is one row of agent_account_balances — agent_postings is not touched — and it is the summed total.
func TestAgentBalance_TenThousandFundingsReadAsOneStoredRow(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	const ws, fundings = "ws-run", 10_000
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAgent(ctx, ws, "busy", "user-run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditLXC(ctx, ws, 1_000_000_000, "top-up", nil); err != nil {
		t.Fatal(err)
	}
	var want int64
	for i := 0; i < fundings; i++ {
		amount := int64(i%97 + 1)
		bal, err := s.FundAgent(ctx, ws, a.ID, amount)
		if err != nil {
			t.Fatalf("funding %d: %v", i, err)
		}
		want += amount
		if bal != want {
			t.Fatalf("funding %d answered a balance of %d, want %d", i, bal, want)
		}
	}
	if _, err := s.WithdrawAgent(ctx, ws, a.ID, 1_000); err != nil {
		t.Fatal(err)
	}
	pot, err := s.CreatePot(ctx, ws, a.ID, "reserve", "reserve", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveToPot(ctx, ws, a.ID, pot.ID, 2_000); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAgentKey(ctx, ws, a.ID, "key-run"); err != nil {
		t.Fatal(err)
	}
	if err := s.SpendLXCForAgent(ctx, "key-run", ws, "req-run", 3_000, "a call", AgentDebitMeta{}); err != nil {
		t.Fatal(err)
	}
	want -= 1_000 + 2_000 + 3_000

	var summed, postings int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint, count(*) FROM agent_postings WHERE workspace_id = $1 AND account = $2`,
		ws, agentAccount(a.ID)).Scan(&summed, &postings); err != nil {
		t.Fatal(err)
	}
	if postings != fundings+3 || summed != want {
		t.Fatalf("the agent has %d postings summing to %d, want %d summing to %d", postings, summed, fundings+3, want)
	}
	book, err := s.AgentBook(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if got := book.Agents[0]; got.BalanceULXC != summed || got.PotsULXC != 2_000 {
		t.Fatalf("the book reads a balance of %d and pots of %d, want %d and 2000", got.BalanceULXC, got.PotsULXC, summed)
	}
	requireReconciled(t, ctx, pool)

	// The read every money move makes, measured: one relation, one row out, and at most one row looked at per
	// stored account — the planner may scan a tiny table rather than use its key — never one per posting.
	var accounts float64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_account_balances`).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	var plan []struct {
		Plan explainNode `json:"Plan"`
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+balanceSQL, ws, agentAccount(a.ID)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
		t.Fatalf("plan %s: %v", raw, err)
	}
	scans := plan[0].Plan.scans()
	if len(scans) != 1 || scans[0].Relation != "agent_account_balances" || scans[0].ActualRows != 1 ||
		scans[0].ActualRows+scans[0].RowsRemoved > accounts {
		t.Fatalf("the balance read scanned %+v, want one row of agent_account_balances (%v accounts) and nothing else\n%s", scans, accounts, raw)
	}
}

type explainNode struct {
	Relation    string        `json:"Relation Name"`
	ActualRows  float64       `json:"Actual Rows"`
	RowsRemoved float64       `json:"Rows Removed by Filter"`
	Plans       []explainNode `json:"Plans"`
}

// scans is every node of the plan that reads a table.
func (n explainNode) scans() []explainNode {
	var out []explainNode
	if n.Relation != "" {
		out = append(out, explainNode{Relation: n.Relation, ActualRows: n.ActualRows, RowsRemoved: n.RowsRemoved})
	}
	for _, c := range n.Plans {
		out = append(out, c.scans()...)
	}
	return out
}

// Postings written before 0185 existed are counted by its backfill, and the trigger counts the ones after it,
// once each: a schema migrated to 0184 with an agent's history in it reads the right balances after 0185.
func TestAgentBalance_MigrationBackfillsPostingsWrittenBeforeIt(t *testing.T) {
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_balances_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	before := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") && e.Name() < "0185" {
			b, err := fs.ReadFile(migrations.FS, e.Name())
			if err != nil {
				t.Fatal(err)
			}
			before[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, before); err != nil {
		t.Fatal(err)
	}
	// An agent's history as 0184 left it: funded twice, a withdrawal, a spend, money in a pot.
	for i, e := range []struct {
		kind string
		legs []leg
	}{
		{"fund", []leg{{"workspace", -5_000}, {"agent:agt_old", 5_000}}},
		{"fund", []leg{{"workspace", -7_000}, {"agent:agt_old", 7_000}}},
		{"withdraw", []leg{{"agent:agt_old", -1_500}, {"workspace", 1_500}}},
		{"spend", []leg{{"agent:agt_old", -2_500}, {"spend", 2_500}}},
		{"pot_in", []leg{{"agent:agt_old", -3_000}, {"pot:pot_old", 3_000}}},
	} {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := postEntry(ctx, tx, "ws-old", e.kind, fmt.Sprintf("old-%d", i), e.legs...); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// One more posting after 0185: the trigger adds it to the backfilled row.
	if err := postEntry(ctx, tx, "ws-old", "fund", "new", leg{"workspace", -400}, leg{"agent:agt_old", 400}); err != nil {
		t.Fatal(err)
	}
	agent, err := accountBalance(ctx, tx, "ws-old", "agent:agt_old")
	if err != nil {
		t.Fatal(err)
	}
	pot, err := accountBalance(ctx, tx, "ws-old", "pot:pot_old")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if agent != 5_000+7_000-1_500-2_500-3_000+400 || pot != 3_000 {
		t.Fatalf("after 0185 the agent reads %d and its pot %d, want 5400 and 3000", agent, pot)
	}
	var stored int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM agent_account_balances`).Scan(&stored); err != nil || stored != 2 {
		t.Fatalf("%d stored balances (%v), want 2: the agent and its pot, not the shared sides", stored, err)
	}
	requireReconciled(t, ctx, conn)
}
