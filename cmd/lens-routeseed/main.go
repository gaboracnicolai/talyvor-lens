// Command lens-routeseed submits attributed routing PREDICTIONS (Proof-of-Improvement piece 3, PR-1)
// from an operator-supplied JSONL file into routing_predictions (migration 0070). A prediction is
// "workspace_id asserts: for cohort (feature_category, input_token_range, complexity_bucket?), route to
// model M." Operator action — NO public HTTP route, like lens-benchseed.
//
// Each line: {workspace_id, feature_category, input_token_range, complexity_bucket?, model, provider?}.
// Submission is gated by LENS_ROUTING_PREDICTION_ENABLED (the capability flag) — with it off, the store
// refuses every submission and the table stays empty. Predictions land 'pending' (operator validates
// them to 'active' out of band). One LIVE prediction per (workspace, cohort): a duplicate is rejected.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/routingpredict"
)

type predLine struct {
	WorkspaceID      string `json:"workspace_id"`
	FeatureCategory  string `json:"feature_category"`
	InputTokenRange  string `json:"input_token_range"`
	ComplexityBucket string `json:"complexity_bucket"`
	Model            string `json:"model"`
	Provider         string `json:"provider"`
}

func main() {
	file := flag.String("file", "", "path to a JSONL file of {workspace_id, feature_category, input_token_range, complexity_bucket?, model, provider?}")
	dryRun := flag.Bool("dry-run", false, "parse + validate without writing")
	flag.Parse()
	if *file == "" {
		fatalf("-file is required")
	}
	ctx := context.Background()

	// The capability flag — submission is refused when off (the store enforces it too; this mirrors the
	// server's parse so the operator tool honors the same gate).
	enabled := func() bool {
		b, _ := strconv.ParseBool(os.Getenv("LENS_ROUTING_PREDICTION_ENABLED"))
		return b
	}

	var store *routingpredict.Store
	if !*dryRun {
		dburl := os.Getenv("LENS_DATABASE_URL")
		if dburl == "" {
			fatalf("LENS_DATABASE_URL is required")
		}
		if !enabled() {
			fatalf("LENS_ROUTING_PREDICTION_ENABLED is false — submission is disabled (set it true to seed predictions)")
		}
		pool, err := pgxpool.New(ctx, dburl)
		if err != nil {
			fatalf("db: %v", err)
		}
		defer pool.Close()
		store = routingpredict.NewStore(pool, enabled)
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
		var it predLine
		if err := json.Unmarshal(raw, &it); err != nil {
			fatalf("line %d: invalid JSON: %v", line, err)
		}
		if it.WorkspaceID == "" || it.FeatureCategory == "" || it.InputTokenRange == "" || it.Model == "" {
			fatalf("line %d: workspace_id, feature_category, input_token_range and model are required", line)
		}
		if *dryRun {
			n++
			continue
		}
		if _, err := store.SubmitPrediction(ctx, routingpredict.Prediction{
			WorkspaceID: it.WorkspaceID, FeatureCategory: it.FeatureCategory, InputTokenRange: it.InputTokenRange,
			ComplexityBucket: it.ComplexityBucket, Model: it.Model, Provider: it.Provider,
		}); err != nil {
			fatalf("line %d: submit: %v", line, err)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		fatalf("read: %v", err)
	}
	verb := "submitted"
	if *dryRun {
		verb = "validated (dry-run)"
	}
	fmt.Printf("routeseed: %s %d routing predictions\n", verb, n)
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "lens-routeseed: "+format+"\n", a...)
	os.Exit(1)
}
