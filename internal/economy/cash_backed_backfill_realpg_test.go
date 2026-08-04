package economy

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// THE BACKFILL RULE, VERIFIED RATHER THAN INHERITED.
//
// Migration 0115 seeds cash_backed_ulxc as min(balance, completed purchases). That rule was proposed
// with the claim "exact on current production". Exactness is a property of the SHAPE of a history,
// not of one deployment, so this establishes which shapes it is exact on and which it is not —
// against the real formula, run by Postgres, on constructed rows.
//
// It matters because the rule runs ONCE. After the migration the column is maintained incrementally,
// so only histories that exist AT MIGRATION TIME are ever subject to it.

// backfillPool provisions its OWN migrated database.
//
// ⚠ NOT THE SHARED lens_test. Other tests in this package hand-roll a minimal schema into it, so a
// test that needs the REAL migrated shape (lxc_purchases exists, with its constraints) is racing
// something it does not control — the observed failure is "relation lxc_purchases does not exist".
// The rule under test is a MIGRATION's expression, so it has to run against a migrated schema.
func backfillPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_backfill_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = ac.Close(ctx)
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	mc, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		_ = mc.Close(ctx)
		t.Fatal(err)
	}
	_ = mc.Close(ctx)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = c.Close(context.Background())
	})
	return pool
}

// backfilled runs migration 0115's expression verbatim for one workspace.
func backfilled(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var v int64
	if err := pool.QueryRow(context.Background(), `
		SELECT GREATEST(0, LEAST(
		         b.balance::BIGINT,
		         COALESCE((SELECT SUM(p.lxc_amount)::BIGINT FROM lxc_purchases p
		                    WHERE p.workspace_id = b.workspace_id AND p.status = 'completed'), 0)))
		  FROM lxc_balances b WHERE b.workspace_id = $1`, ws).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func seedShape(t *testing.T, pool *pgxpool.Pool, ws string, balance int64, purchases []int64, status string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO lxc_balances (workspace_id, balance) VALUES ($1,$2)
		 ON CONFLICT (workspace_id) DO UPDATE SET balance = EXCLUDED.balance`, ws, balance); err != nil {
		t.Fatal(err)
	}
	for i, amt := range purchases {
		if _, err := pool.Exec(ctx,
			`INSERT INTO lxc_purchases (workspace_id, stripe_session_id, stripe_event_id, lxc_amount, usd_cents, status)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			ws, fmt.Sprintf("sess-%s-%d", ws, i), fmt.Sprintf("evt-%s-%d", ws, i), amt, 100, status); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackfillRule_ExactAndInexactShapes(t *testing.T) {
	pool := backfillPool(t)
	salt := time.Now().UnixNano()
	id := func(s string) string { return fmt.Sprintf("bf-%s-%d", s, salt) }

	cases := []struct {
		name      string
		balance   int64
		purchases []int64
		status    string
		wantTrue  int64 // the TRUE cash-backed figure under unbacked-first
		exact     bool
	}{
		// Purchases only, unspent: backing is the whole balance.
		{"purchases-only-unspent", 100, []int64{100}, "completed", 100, true},
		// Purchases only, partly spent: all spend must have consumed cash, so backing == balance.
		{"purchases-only-spent", 40, []int64{100}, "completed", 40, true},
		// Grants only: no purchase rows at all.
		{"grants-only", 100, nil, "completed", 0, true},
		// Refunded purchase: the cash left the system, so it backs nothing.
		{"purchase-refunded", 100, []int64{100}, "refunded", 0, true},
		// Mixed, grant arrived BEFORE the spending: unbacked absorbed the spend, cash intact.
		// purchase 100 + grant 100 = 200, spent 50 (all from the grant) ⇒ balance 150, backing 100.
		{"mixed-grant-first", 150, []int64{100}, "completed", 100, true},
		// ⚠ THE ONE INEXACT SHAPE: cash spent BEFORE unbacked credit arrived.
		// purchase 100, spend 50 (from cash — nothing else existed), THEN grant 100.
		// balance 150, true backing 50; the rule restores it to 100.
		{"mixed-spend-then-grant", 150, []int64{100}, "completed", 50, false},
	}

	for _, c := range cases {
		ws := id(c.name)
		seedShape(t, pool, ws, c.balance, c.purchases, c.status)
		got := backfilled(t, pool, ws)
		switch {
		case c.exact && got != c.wantTrue:
			t.Errorf("%s: backfill = %d, TRUE cash-backed = %d — the rule is claimed exact on this shape",
				c.name, got, c.wantTrue)
		case !c.exact && got <= c.wantTrue:
			t.Errorf("%s: backfill = %d, TRUE = %d — this shape is documented as OVER-crediting; if it no "+
				"longer does, migration 0115's comment is wrong and should be corrected", c.name, got, c.wantTrue)
		case !c.exact:
			t.Logf("%s: backfill = %d vs TRUE %d — over-credits by %d, as documented. This is the ONLY "+
				"inexact shape, and it requires cash spent BEFORE unbacked credit arrived.",
				c.name, got, c.wantTrue, got-c.wantTrue)
		}
	}
}
