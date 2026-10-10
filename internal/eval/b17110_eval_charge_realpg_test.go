package eval

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/quality"
	"github.com/talyvor/lens/migrations"
)

// B17.110 — every eval run that asks a model is a charge on the workspace's ledger. The testers saw a run of a
// tag and a dataset run each state a cost and leave lxc_ledger untouched: Talyvor paid the provider and billed
// no one. A migrated schema, the real DualTokenStore, a provider that answers; asserted on the ledger rows.

func b17110DB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG eval charge test")
	}
	// Its own schema: planted_case_execution_test.go hand-writes eval tables in this package's.
	const schema = "lens_pkg_eval_b17110"
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

func TestB17110_EachEvalRunIsOneChargeOnTheWorkspaceLedger(t *testing.T) {
	pool := b17110DB(t)
	ctx := context.Background()
	const ws, funded = "ws-evals", int64(10_000_000) // 10 LXC, cash-backed
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, $2, $2)`, ws, funded); err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK"}]}`))
	}))
	t.Cleanup(provider.Close)
	p := New(pool, quality.New(nil), "", "k", "")
	p.anthropicURL = provider.URL
	store := economy.NewDualTokenStore(nil, pool, nil)
	p.SetLXCCharger(store)

	tc := TestCase{Name: "b17110", WorkspaceID: ws, Provider: "anthropic", Model: "claude-haiku-4-5",
		Prompt: strings.Repeat("Reply OK. ", 40), ExpectedOutput: "OK", EvalMethod: EvalContains, PassThreshold: 1, Tags: []string{"b17110"}}
	if _, err := p.AddTestCase(ctx, tc); err != nil {
		t.Fatal(err)
	}
	ds, err := p.CreateDataset(ctx, Dataset{WorkspaceID: ws, Name: "b17110"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AddDatasetCase(ctx, ds.ID, tc); err != nil {
		t.Fatal(err)
	}

	suite, err := p.RunSuite(ctx, ws, []string{"b17110"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := p.RunEval(ctx, ws, ds.ID, Target{MaxCostUSD: 0.05})
	if err != nil {
		t.Fatal(err)
	}

	var spent int64
	for _, r := range []struct {
		what, runID string
		costUSD     float64
	}{{"the run of the tag", suite.RunID, suite.TotalCostUSD}, {"the dataset run", run.Summary.RunID, run.Summary.TotalCostUSD}} {
		want := int64(math.Ceil(r.costUSD / economy.LXCUSDValue * 1e6))
		if want <= 0 {
			t.Fatalf("%s states it cost $%v — nothing to charge, so this measures nothing", r.what, r.costUSD)
		}
		var rows int
		var amount int64
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(SUM(amount), 0) FROM lxc_ledger
			WHERE workspace_id = $1 AND type = 'spend' AND description = $2`, ws, "eval: run "+r.runID).Scan(&rows, &amount); err != nil {
			t.Fatal(err)
		}
		if rows != 1 || amount != -want {
			t.Errorf("%s ($%v) wrote %d spend row(s) totalling %d µLXC, want one of %d", r.what, r.costUSD, rows, amount, -want)
		}
		spent += want
	}
	bal, err := store.GetLXCBalance(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if bal != funded-spent {
		t.Errorf("balance = %d µLXC, want %d (funded %d less the two runs' %d)", bal, funded-spent, funded, spent)
	}
}
