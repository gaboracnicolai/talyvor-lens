package storedanswers

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// B27.13: on 200,000 stored answers, the stored-answers screen (Counts) and deleting them (Delete,
// DeleteAllOf) reach a workspace's rows through migrations 0179/0180's indexes, never a scan of the
// whole vector table — in the custom plan and in the generic plan a cached prepared statement settles on.
func TestStoredAnswerQueries_UseWorkspaceIndexes_On200kRows(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG stored-answers index test")
	}
	const schema = "storedanswers_indexes_realpg"
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	exec(`CREATE SCHEMA ` + schema)
	t.Cleanup(func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`) })
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 2,000 workspaces with 100 answers each, half shared and half private.
	exec(`INSERT INTO prompt_embeddings (provider, model, prompt_hash, response, is_poolable, contributor_workspace_id, workspace_id)
		SELECT 'anthropic', 'claude', 'h' || i, 'r', i % 2 = 0,
		       CASE WHEN i % 2 = 0 THEN 'ws-' || (i % 2000) END,
		       CASE WHEN i % 2 = 1 THEN 'ws-' || (i % 2000) END
		FROM generate_series(1, 200000) AS i`)
	exec(`ANALYZE prompt_embeddings`)

	const (
		shared  = "idx_prompt_embeddings_shared_contributor"
		private = "idx_prompt_embeddings_private_workspace"
	)
	for _, q := range []struct {
		name, paramType, sql, arg string
		want                      []string
	}{
		{"counts", "text", countAnswersSQL, "'ws-7'", []string{shared, private}},
		{"delete_shared", "text", deleteSharedSQL, "'ws-7'", []string{shared}},
		{"delete_private", "text", deletePrivateSQL, "'ws-7'", []string{private}},
		{"delete_all_of", "text[]", deleteAllOfSQL, "ARRAY['ws-7','ws-8']", []string{shared, private}},
	} {
		exec(`PREPARE ` + q.name + `(` + q.paramType + `) AS ` + q.sql)
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			exec(`SET plan_cache_mode = ` + mode)
			var raw []byte
			if err := conn.QueryRow(ctx, `EXPLAIN (FORMAT JSON) EXECUTE `+q.name+`(`+q.arg+`)`).Scan(&raw); err != nil {
				t.Fatalf("%s/%s explain: %v", q.name, mode, err)
			}
			var plan []struct{ Plan map[string]any }
			if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
				t.Fatalf("%s/%s: decode plan %s: %v", q.name, mode, raw, err)
			}
			nodes := map[string]bool{}
			indexes := map[string]bool{}
			walkPlan(plan[0].Plan, nodes, indexes)
			if nodes["Seq Scan"] {
				t.Errorf("%s/%s scans the whole table:\n%s", q.name, mode, raw)
			}
			for _, idx := range q.want {
				if !indexes[idx] {
					t.Errorf("%s/%s does not use %s:\n%s", q.name, mode, idx, raw)
				}
			}
		}
	}
}

// walkPlan records every node type and index name in an EXPLAIN (FORMAT JSON) plan tree.
func walkPlan(n map[string]any, nodes, indexes map[string]bool) {
	if t, ok := n["Node Type"].(string); ok {
		nodes[t] = true
	}
	if idx, ok := n["Index Name"].(string); ok {
		indexes[idx] = true
	}
	children, _ := n["Plans"].([]any)
	for _, c := range children {
		if m, ok := c.(map[string]any); ok {
			walkPlan(m, nodes, indexes)
		}
	}
}
