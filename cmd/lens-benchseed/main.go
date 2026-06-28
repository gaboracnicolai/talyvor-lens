// Command lens-benchseed populates the verifier-PRIVATE proof-of-benchmark eval pool
// (benchmark_eval_items, migration 0068) from an operator-supplied JSONL file. Operator action — NO
// tenant input, NO flag. Each line: {id?, input, expected_output, eval_method?, pass_threshold?}.
// eval_method ∈ {exact, contains, regex, json_schema} (default exact). The pool is verifier-private:
// expected_output is held here and is never sent to a node.
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
)

type seedItem struct {
	ID             string  `json:"id"`
	Input          string  `json:"input"`
	ExpectedOutput string  `json:"expected_output"`
	EvalMethod     string  `json:"eval_method"`
	PassThreshold  float64 `json:"pass_threshold"`
}

func main() {
	file := flag.String("file", "", "path to a JSONL file of {id?, input, expected_output, eval_method?, pass_threshold?}")
	dryRun := flag.Bool("dry-run", false, "parse + validate without writing")
	flag.Parse()
	if *file == "" {
		fatalf("-file is required")
	}
	ctx := context.Background()
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
		if err := store.SeedItem(ctx, benchprobe.EvalItem{
			ID: it.ID, Input: it.Input, ExpectedOutput: it.ExpectedOutput,
			EvalMethod: it.EvalMethod, PassThreshold: it.PassThreshold,
		}); err != nil {
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
