// Command lens-benchseed populates the verifier-PRIVATE proof-of-benchmark eval pool
// (benchmark_eval_items) from an operator-supplied JSONL file. Operator action — NO tenant input. Each
// line: {id?, input, expected_output, eval_method?, pass_threshold?, feature_category?}. eval_method ∈
// {exact, contains, regex, json_schema} (default exact). The pool is verifier-private: expected_output is
// held here and never sent to a node.
//
// COHORT TAGGING (PR-2): for each item, input_token_range + complexity_bucket are DERIVED from the input
// via internal/cohort.DeriveInputCohort (the SAME exported serve-path functions), and feature_category is
// taken from the line (operator-declared, like the serve-time X-Talyvor-Feature header). The derivation
// lives HERE (the non-guarded tool), not in benchprobe (which stays mint-free and just stores the strings).
// `-backfill` re-derives the two derived dims onto already-seeded rows from their stored input.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/benchprobe"
	"github.com/talyvor/lens/internal/cohort"
)

type seedItem struct {
	ID              string  `json:"id"`
	Input           string  `json:"input"`
	ExpectedOutput  string  `json:"expected_output"`
	EvalMethod      string  `json:"eval_method"`
	PassThreshold   float64 `json:"pass_threshold"`
	FeatureCategory string  `json:"feature_category"`
}

func main() {
	file := flag.String("file", "", "path to a JSONL file of {id?, input, expected_output, eval_method?, pass_threshold?, feature_category?}")
	dryRun := flag.Bool("dry-run", false, "parse + validate without writing")
	author := flag.String("author", "", "contributor workspace_id — when set, items are CONTRIBUTED (pending validation, "+
		"exact-deduped, author-excluded from grading) for the proof-of-eval-contribution mint, instead of operator-seeded (active)")
	backfill := flag.Bool("backfill", false, "re-derive input_token_range + complexity_bucket for already-seeded rows missing them "+
		"(from their stored input; feature_category is NOT derived — it stays NULL until re-seeded). Ignores -file.")
	flag.Parse()
	ctx := context.Background()

	// -backfill: tag legacy/untagged rows with the two DERIVED cohort dims from their stored input.
	if *backfill {
		dburl := os.Getenv("LENS_DATABASE_URL")
		if dburl == "" {
			fatalf("LENS_DATABASE_URL is required")
		}
		pool, err := pgxpool.New(ctx, dburl)
		if err != nil {
			fatalf("db: %v", err)
		}
		defer pool.Close()
		store := benchprobe.NewStore(pool)
		todo, err := store.ItemsMissingCohort(ctx, 100000)
		if err != nil {
			fatalf("backfill scan: %v", err)
		}
		for _, row := range todo {
			ir, cb := cohort.DeriveInputCohort(row.Input)
			if err := store.BackfillCohort(ctx, row.ID, ir, cb); err != nil {
				fatalf("backfill %s: %v", row.ID, err)
			}
		}
		fmt.Printf("benchseed: backfilled cohort on %d items\n", len(todo))
		return
	}

	if *file == "" {
		fatalf("-file is required (or -backfill)")
	}
	// Dry-run is parse + validate only — no DB needed. Live seeding requires LENS_DATABASE_URL.
	var store *benchprobe.Store
	if !*dryRun {
		dburl := os.Getenv("LENS_DATABASE_URL")
		if dburl == "" {
			fatalf("LENS_DATABASE_URL is required")
		}
		pool, err := pgxpool.New(ctx, dburl)
		if err != nil {
			fatalf("db: %v", err)
		}
		defer pool.Close()
		store = benchprobe.NewStore(pool)
	}

	f, err := os.Open(*file)
	if err != nil {
		fatalf("open %s: %v", *file, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	var n, line int
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var it seedItem
		if err := json.Unmarshal(raw, &it); err != nil {
			fatalf("line %d: invalid JSON: %v", line, err)
		}
		if it.Input == "" || it.ExpectedOutput == "" {
			fatalf("line %d: input and expected_output are required", line)
		}
		if it.ID == "" {
			it.ID = fmt.Sprintf("item-%d", line)
		}
		if *dryRun {
			n++
			continue
		}
		// Cohort tag: derive the two input-derived dims via the SHARED serve-path functions; feature is
		// operator-declared (line field). benchprobe just stores these strings.
		inputRange, complexityBucket := cohort.DeriveInputCohort(it.Input)
		item := benchprobe.EvalItem{
			ID: it.ID, Input: it.Input, ExpectedOutput: it.ExpectedOutput,
			EvalMethod: it.EvalMethod, PassThreshold: it.PassThreshold,
			FeatureCategory: it.FeatureCategory, InputTokenRange: inputRange, ComplexityBucket: complexityBucket,
		}
		if *author != "" {
			// CONTRIBUTED: lands pending (not drawable until validated), exact-deduped on content_hash,
			// author-attributed so the contributor is excluded from grading/earning on their own item.
			item.AuthorWorkspaceID = *author
			if err := store.ContributeItem(ctx, item); err != nil {
				fatalf("line %d: contribute: %v", line, err)
			}
		} else if err := store.SeedItem(ctx, item); err != nil {
			fatalf("line %d: seed: %v", line, err)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		fatalf("read: %v", err)
	}
	verb := "seeded"
	if *dryRun {
		verb = "validated (dry-run)"
	}
	fmt.Printf("benchseed: %s %d eval items\n", verb, n)
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "lens-benchseed: "+format+"\n", a...)
	os.Exit(1)
}
