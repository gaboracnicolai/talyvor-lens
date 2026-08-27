package learner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// token_events.prompt_hash HAS NO WRITER, AND THIS PACKAGE GROUPED BY IT.
//
// ⚠ MEASURED, NOT READ (2026-08-27, real Postgres on the full migrations/*.sql chain, 123
// migrations): seven calls to the production writer alerts.RecordSpend carrying THREE distinct
// prompts produced seven rows and exactly ONE distinct prompt_hash — the empty string, the
// column's NOT NULL DEFAULT ''. analyseSQL's `GROUP BY prompt_hash` therefore collapsed every
// row in the seven-day window into a single group, and Analyse returned ONE insight:
//
//	{PromptPattern:"" HitCount:14 AvgTokensSaved:150
//	 Recommendation:"Cache this pattern — seen 14 times, saves ~150 tokens per hit"}
//
// HitCount is not a repetition count. It is the total number of non-cache-served requests in the
// window, presented as one cacheable pattern whose pattern is the empty string. Two customer-
// facing surfaces render it verbatim — internal/api/server.go handleModelsRecommendations
// (GET /v1/api/models/recommendations) and internal/mcp/server.go toolGetModelRecommendations —
// each multiplying that count into estimated_monthly_savings_usd.
//
// ⚠ THIS IS THE FOURTH INSTANCE OF THE DEFECT MIGRATION 0114 DOCUMENTS, AND THE SECOND IN THIS
// FILE. The first pass here fixed `AND cached = false` (an inert predicate on a writerless
// BOOLEAN) and left the GROUP BY on a writerless TEXT column one line below it, because
// internal/catalog/writerless_column_guard_test.go — the guard that found the `cached` instance —
// cannot see this one for two independent reasons: its writerlessColumns list is hand-enumerated
// and does not name prompt_hash, and its predicateUse regex matches FILTER/WHERE/AND/SUM/AVG but
// not GROUP BY. Migration 0114 states that guard "fails the build if a fourth appears"; this is
// the fourth, and it did not.
//
// ⚠ AND THE EXISTING UNIT TESTS PASS *BECAUSE* THEY MOCK: TestLearner_AnalyseReturnsInsights
// SortedByHitCountDesc hands pgxmock three distinct prompt_hash values that no production writer
// can ever produce, so the mock demonstrates the function working on data the database cannot
// hold. That is why this file goes to a real database.

// TestNoProductionWriterSetsTokenEventsPromptHash is the premise every other claim here rests on,
// and it needs no database: it censuses the tree for statements that write token_events and
// checks whether any of them names prompt_hash.
//
// ⚠ POSITIVE CONTROL IS BUILT IN AND IS THE POINT. A census that finds nothing because its parser
// is broken looks exactly like a census that finds nothing because nothing is there. So the same
// parse must FIND the columns that ARE written (workspace_id, cost_usd); if it does not, the
// census is blind and this test fails for that reason instead.
func TestNoProductionWriterSetsTokenEventsPromptHash(t *testing.T) {
	root := "../.."
	stmt := regexp.MustCompile(`(?is)(?:INSERT\s+INTO|UPDATE)\s+token_events\b(.{0,600})`)

	type write struct {
		file string
		cols string
	}
	var writes []write

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "migrations":
				return filepath.SkipDir
			}
			return nil
		}
		// Production Go only. Tests seed prompt_hash by hand precisely because
		// production does not, so including them would hide the finding.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, m := range stmt.FindAllStringSubmatch(string(b), -1) {
			writes = append(writes, write{file: path, cols: m[1]})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(writes) == 0 {
		t.Fatal("census found NO production writer of token_events at all — the parser is blind, " +
			"not the codebase empty; fix this test before trusting any claim built on it")
	}

	// POSITIVE CONTROL: columns that demonstrably DO have a writer must be found.
	for _, ctrl := range []string{"workspace_id", "cost_usd"} {
		found := false
		for _, w := range writes {
			if strings.Contains(w.cols, ctrl) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("positive control failed: no token_events writer names %q, but one demonstrably "+
				"does — this census cannot distinguish written from writerless columns", ctrl)
		}
	}

	for _, w := range writes {
		if strings.Contains(w.cols, "prompt_hash") {
			t.Fatalf("%s now writes token_events.prompt_hash. That is good news and it INVALIDATES "+
				"this file: delete the `prompt_hash <> ''` guard in analyseSQL and re-derive what "+
				"Analyse should return.", w.file)
		}
	}
	t.Logf("censused %d production writer(s) of token_events; none names prompt_hash", len(writes))
}

// TestAnalyseDoesNotFabricateAPatternFromTheWriterlessColumn drives the REAL production writer
// against a real Postgres and asserts Analyse reports nothing it cannot substantiate.
//
// RED BEFORE THE FIX: three distinct prompts recorded through alerts.RecordSpend produce one
// insight with PromptPattern "" and HitCount 9.
// GREEN AFTER: none — and the control rows below prove that "none" is not vacuous.
func TestAnalyseDoesNotFabricateAPatternFromTheWriterlessColumn(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG learner analyse test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// CI never migrates lens_test — the gated tests build their own fixtures (see
	// .github/workflows/ci.yaml). The shape below mirrors migrations/0034 for every column this
	// query touches, and prompt_hash carries the SAME `NOT NULL DEFAULT ''` that makes the
	// defect possible. TestNoProductionWriterSetsTokenEventsPromptHash is what keeps that
	// faithful: if a writer ever appears, it fails and sends the reader back here.
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS token_events (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		workspace_id TEXT NOT NULL DEFAULT 'default',
		provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
		team TEXT, sprint_id TEXT, feature TEXT,
		cost_usd FLOAT NOT NULL DEFAULT 0, prompt_text TEXT NOT NULL DEFAULT '',
		prompt_hash TEXT NOT NULL DEFAULT '',
		session_id TEXT NOT NULL DEFAULT '', request_id TEXT NOT NULL DEFAULT '',
		modality TEXT NOT NULL DEFAULT 'text', cost_estimated BOOLEAN NOT NULL DEFAULT FALSE,
		distill_method TEXT NOT NULL DEFAULT '',
		serve_source TEXT NOT NULL DEFAULT 'upstream',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		t.Fatalf("fixture schema: %v", err)
	}
	for _, ddl := range []string{
		`ALTER TABLE token_events ADD COLUMN IF NOT EXISTS prompt_hash TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE token_events ADD COLUMN IF NOT EXISTS serve_source TEXT NOT NULL DEFAULT 'upstream'`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("fixture align: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `TRUNCATE token_events`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// The production writer's exact column list (internal/alerts insertTokenEventSQL). Reproduced
	// rather than imported so this test states what production writes and would notice a change.
	const productionInsert = `INSERT INTO token_events
	  (workspace_id, provider, model, input_tokens, output_tokens, team, sprint_id, feature,
	   cost_usd, prompt_text, session_id, request_id, modality, cost_estimated, distill_method)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`

	for _, p := range []string{
		"what is the capital of france", "what is the capital of france", "what is the capital of france",
		"summarise this contract", "summarise this contract", "summarise this contract",
		"write a haiku about go", "write a haiku about go", "write a haiku about go",
	} {
		if _, err := pool.Exec(ctx, productionInsert,
			"ws-a", "openai", "gpt-4o", 100, 50, "team", "sprint", "feature",
			0.01, p, "sess", "req", "text", false, ""); err != nil {
			t.Fatalf("seed via production column list: %v", err)
		}
	}

	l := &Learner{pool: pool}
	got, err := l.Analyse(ctx)
	if err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	for _, ins := range got {
		if ins.PromptPattern == "" {
			t.Errorf("Analyse fabricated a pattern from the writerless prompt_hash column: "+
				"%+v — HitCount here is the total request count in the window, not a repetition "+
				"count, and it is multiplied into estimated_monthly_savings_usd on two surfaces", ins)
		}
	}
	if len(got) != 0 {
		t.Errorf("Analyse returned %d insight(s) from 9 rows the production writer created; want 0, "+
			"because production records no prompt identity to group by: %+v", len(got), got)
	}

	// POSITIVE CONTROL — the assertion above must not be vacuous. Rows carrying a REAL
	// prompt_hash (what a future writer would produce) must still be found and grouped, so a
	// mutation that made Analyse return nothing unconditionally fails here.
	for i, h := range []string{"hash-alpha", "hash-alpha", "hash-alpha", "hash-beta", "hash-beta", "hash-beta"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO token_events (workspace_id, provider, model, input_tokens, output_tokens,
			 cost_usd, prompt_text, prompt_hash, modality)
			 VALUES ('ws-a','openai','gpt-4o',100,50,0.01,$1,$2,'text')`,
			fmt.Sprintf("control prompt %d", i), h); err != nil {
			t.Fatalf("control seed: %v", err)
		}
	}
	got2, err := l.Analyse(ctx)
	if err != nil {
		t.Fatalf("Analyse (control): %v", err)
	}
	if len(got2) != 2 {
		t.Fatalf("positive control failed: with two real 3-hit prompt_hash groups present Analyse "+
			"returned %d insight(s), want 2 — the assertion above would have passed for the wrong "+
			"reason: %+v", len(got2), got2)
	}
	for _, ins := range got2 {
		if ins.PromptPattern == "" {
			t.Errorf("control run still surfaced the empty-hash group: %+v", ins)
		}
		if ins.HitCount != 3 {
			t.Errorf("control group %q HitCount = %d, want 3", ins.PromptPattern, ins.HitCount)
		}
	}
}
