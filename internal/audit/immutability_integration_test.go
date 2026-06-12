package audit

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// Real-PG audit-immutability tests (LENS_TEST_DATABASE_URL-gated). They prove the
// 0055 triggers make the five audit tables append-only — UPDATE/DELETE/TRUNCATE
// rejected via the parent AND directly against a partition — while INSERT and the
// two deliberately-mutable tables (lxc_purchases, pool_royalty_mints) still work.

var auditMigrateOnce sync.Once

func auditTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG audit-immutability test")
	}
	ctx := context.Background()
	auditMigrateOnce.Do(func() {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect for migrate: %v", err)
		}
		defer conn.Close(ctx)
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("apply migrations: %v", err)
		}
	})
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func isAppendOnly(err error) bool {
	return err != nil && strings.Contains(err.Error(), "append-only")
}

// guarded describes one append-only table: a minimal INSERT + a way to target the
// seeded row for UPDATE/DELETE. String-keyed seeds use ON CONFLICT DO NOTHING
// because once the triggers are on the row can never be cleaned up.
type guarded struct {
	name      string
	insertSQL string
	keyArg    any
	whereSQL  string
	updateSet string
}

func guardedTables() []guarded {
	return []guarded{
		{"token_events",
			`INSERT INTO token_events (provider,model,input_tokens,output_tokens,workspace_id) VALUES ('p','m',1,1,$1)`,
			"ws_imm_te", "WHERE workspace_id=$1", "SET provider='x'"},
		{"lens_token_ledger",
			`INSERT INTO lens_token_ledger (workspace_id,amount,balance_after,type) VALUES ($1,1,1,'t')`,
			"ws_imm_ll", "WHERE workspace_id=$1", "SET amount=2"},
		{"lxc_ledger",
			`INSERT INTO lxc_ledger (workspace_id,amount,balance_after,type) VALUES ($1,1,1,'t')`,
			"ws_imm_xl", "WHERE workspace_id=$1", "SET amount=2"},
		{"request_attribution",
			`INSERT INTO request_attribution (workspace_id) VALUES ($1)`,
			"ws_imm_ra", "WHERE workspace_id=$1", "SET cost_usd=2"},
		{"povi_receipts",
			`INSERT INTO povi_receipts (request_id,node_id,workspace_id,model,merkle_root,verified,timestamp) VALUES ($1,'n','ws','m','root',true,0) ON CONFLICT (request_id) DO NOTHING`,
			"req_imm_pr", "WHERE request_id=$1", "SET verified=false"},
	}
}

// TestAuditImmutability_BlocksMutation — INSERT allowed; UPDATE/DELETE/TRUNCATE
// rejected (via the parent) on all five guarded tables.
func TestAuditImmutability_BlocksMutation(t *testing.T) {
	pool := auditTestPool(t)
	ctx := context.Background()
	for _, g := range guardedTables() {
		t.Run(g.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, g.insertSQL, g.keyArg); err != nil {
				t.Fatalf("INSERT into %s must succeed (append-only allows insert): %v", g.name, err)
			}
			if _, err := pool.Exec(ctx, "UPDATE "+g.name+" "+g.updateSet+" "+g.whereSQL, g.keyArg); !isAppendOnly(err) {
				t.Errorf("UPDATE %s must be rejected append-only; got err=%v", g.name, err)
			}
			if _, err := pool.Exec(ctx, "DELETE FROM "+g.name+" "+g.whereSQL, g.keyArg); !isAppendOnly(err) {
				t.Errorf("DELETE %s must be rejected append-only; got err=%v", g.name, err)
			}
			if _, err := pool.Exec(ctx, "TRUNCATE "+g.name); !isAppendOnly(err) {
				t.Errorf("TRUNCATE %s (parent) must be rejected append-only; got err=%v", g.name, err)
			}
		})
	}
}

// TestAuditImmutability_BlocksDirectPartitionMutation — the silent-bypass case:
// UPDATE/DELETE/TRUNCATE issued DIRECTLY against a hash partition (not the parent)
// must still be rejected. Row triggers auto-cascade; the TRUNCATE trigger was
// enumerated per-partition in 0055.
func TestAuditImmutability_BlocksDirectPartitionMutation(t *testing.T) {
	pool := auditTestPool(t)
	ctx := context.Background()
	const ws = "ws_imm_part"
	if _, err := pool.Exec(ctx,
		`INSERT INTO token_events (provider,model,input_tokens,output_tokens,workspace_id) VALUES ('p','m',1,1,$1)`, ws); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var part string
	if err := pool.QueryRow(ctx,
		`SELECT tableoid::regclass::text FROM token_events WHERE workspace_id=$1 LIMIT 1`, ws).Scan(&part); err != nil {
		t.Fatalf("find partition: %v", err)
	}
	t.Logf("row landed in partition %s", part)
	if _, err := pool.Exec(ctx, "UPDATE "+part+" SET provider='x' WHERE workspace_id=$1", ws); !isAppendOnly(err) {
		t.Errorf("direct-partition UPDATE on %s must be rejected; got err=%v", part, err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM "+part+" WHERE workspace_id=$1", ws); !isAppendOnly(err) {
		t.Errorf("direct-partition DELETE on %s must be rejected; got err=%v", part, err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE "+part); !isAppendOnly(err) {
		t.Errorf("direct-partition TRUNCATE on %s must be rejected (per-partition trigger); got err=%v", part, err)
	}
}

// TestAuditImmutability_NegativeControls — the two deliberately-mutable tables are
// NOT guarded: the lxc_purchases refund mark and the pool_royalty_mints status CAS
// must still succeed.
func TestAuditImmutability_NegativeControls(t *testing.T) {
	pool := auditTestPool(t)
	ctx := context.Background()

	const pi = "pi_imm_neg"
	if _, err := pool.Exec(ctx,
		`INSERT INTO lxc_purchases (stripe_event_id, workspace_id, usd_cents, stripe_payment_intent, lxc_amount)
		 VALUES ('evt_imm_neg','ws',1000,$1,100) ON CONFLICT (stripe_event_id) DO NOTHING`, pi); err != nil {
		t.Fatalf("seed lxc_purchases: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE lxc_purchases SET status='refunded', refunded_at=NOW() WHERE stripe_payment_intent=$1`, pi); err != nil {
		t.Errorf("lxc_purchases refund UPDATE must still work (not guarded): %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO pool_royalty_mints (request_id, requester_workspace_id, contributor_workspace_id, layer)
		 VALUES ('req_imm_neg','rw','cw','exact') ON CONFLICT (request_id) DO NOTHING`); err != nil {
		t.Fatalf("seed pool_royalty_mints: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE pool_royalty_mints SET status='final' WHERE request_id='req_imm_neg'`); err != nil {
		t.Errorf("pool_royalty_mints status CAS UPDATE must still work (not guarded): %v", err)
	}
}
