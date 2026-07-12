package keel_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/keel"
	"github.com/talyvor/lens/migrations"
)

// Real-PG breach assertions for the tenancy boundary. LENS has NO local .semgrep operate-by-id guard, so
// THESE TESTS ARE THE GUARD. Gated on LENS_TEST_DATABASE_URL (mirrors the audit / aggregate-cohorts harness).
var keelMigrateOnce sync.Once

func keelTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG keel breach test")
	}
	ctx := context.Background()
	keelMigrateOnce.Do(func() {
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

// w1/w2 are two window-1 / window-2 timestamps 2h apart → distinct hourly buckets.
var (
	w1 = time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	w2 = time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
)

// seedPattern reuses the routing_patterns INSERT shape (the Gap-3 fixture pattern) + a controlled
// created_at for windowing. provider is per-test-unique to isolate the cohort in the shared corpus.
func seedPattern(t *testing.T, pool *pgxpool.Pool, ws, provider, model string, quality float64, optedIn bool, at time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO routing_patterns
		   (workspace_id, feature_category, model_used, provider_used, input_token_range, latency_bucket, output_quality, opted_in, created_at)
		 VALUES ($1, 'chat', $2, $3, 'medium', 'fast', $4, $5, $6)`,
		ws, model, provider, quality, optedIn, at)
	if err != nil {
		t.Fatalf("seed pattern: %v", err)
	}
}

func cleanup(t *testing.T, pool *pgxpool.Pool, provider string) {
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM routing_patterns WHERE provider_used = $1`, provider)
		_, _ = pool.Exec(ctx, `DELETE FROM keel_findings WHERE unit LIKE $1`, provider+"/%")
	})
}

// BREACH 1 — FLOOR HOLDS: a cohort of only 2 distinct opted-in workspaces (N-1, below the ≥3 floor)
// yields NO finding — a lone other tenant's value is unrecoverable through Keel.
func TestBreach_FloorHolds(t *testing.T) {
	pool := keelTestPool(t)
	const provider = "keelt_floor"
	cleanup(t, pool, provider)
	// 2 opted-in workspaces, one drifts hard — but below the floor.
	seedPattern(t, pool, "ws1", provider, "m", 0.8, true, w1)
	seedPattern(t, pool, "ws2", provider, "m", 0.8, true, w1)
	seedPattern(t, pool, "ws1", provider, "m", 0.1, true, w2) // hard drift
	seedPattern(t, pool, "ws2", provider, "m", 0.8, true, w2)

	obs, err := keel.NewReader(pool).CohortObservations(context.Background(), 3600, w1.Add(-time.Hour))
	if err != nil {
		t.Fatalf("observations: %v", err)
	}
	for _, f := range keel.Detect(obs, keel.DefaultConfig()) {
		if f.Unit == provider+"/m" {
			t.Errorf("below-floor cohort (2 workspaces) produced a finding: %+v", f)
		}
	}
}

// BREACH 2 — OPT-IN EXCLUDED: opted-OUT workspaces never enter the cohort stats and never receive a
// finding; their raw quality is in no observation.
func TestBreach_OptInExcluded(t *testing.T) {
	pool := keelTestPool(t)
	const provider = "keelt_optin"
	cleanup(t, pool, provider)
	// 3 opted-IN (stable ~0.8) + 2 opted-OUT (wild 0.01) in the SAME unit + windows.
	for _, ws := range []string{"in1", "in2", "in3"} {
		seedPattern(t, pool, ws, provider, "m", 0.8, true, w1)
		seedPattern(t, pool, ws, provider, "m", 0.8, true, w2)
	}
	for _, ws := range []string{"out1", "out2"} {
		seedPattern(t, pool, ws, provider, "m", 0.01, false, w1) // opted OUT
		seedPattern(t, pool, ws, provider, "m", 0.01, false, w2)
	}
	obs, err := keel.NewReader(pool).CohortObservations(context.Background(), 3600, w1.Add(-time.Hour))
	if err != nil {
		t.Fatalf("observations: %v", err)
	}
	for _, o := range obs {
		if o.Unit == provider+"/m" {
			if o.WorkspaceID == "out1" || o.WorkspaceID == "out2" {
				t.Errorf("opted-out workspace %s present in cohort observations (raw value would leak): %+v", o.WorkspaceID, o)
			}
			if o.MeanQuality < 0.5 {
				t.Errorf("opted-out low quality (0.01) contaminated the cohort mean: %+v", o)
			}
		}
	}
}

// countFindings returns how many keel_findings rows exist for a unit, and how many are idiosyncratic,
// asserting along the way that NO row names a non-self workspace (a counterparty leak).
func countFindings(t *testing.T, pool *pgxpool.Pool, unit string, self map[string]bool) (total, idiosyncratic int) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT workspace_id, attribution FROM keel_findings WHERE unit=$1`, unit)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var ws, attr string
		if err := rows.Scan(&ws, &attr); err != nil {
			t.Fatal(err)
		}
		if !self[ws] {
			t.Errorf("finding on %q names a non-self workspace %q — counterparty leak", unit, ws)
		}
		total++
		if attr == keel.AttributionIdiosyncratic {
			idiosyncratic++
		}
	}
	return total, idiosyncratic
}

// BREACH 3+4 — QUERY-ONLY / NEVER-ACTS + NO-RAW-COUNTERPARTY, proven at BOTH thresholds pinned as EXPLICIT
// LITERALS on the SAME seeded data (independent of DefaultConfig): (A) at 3.0σ the tenancy boundary holds
// AND — documented explicitly — the drifter (~2.2σ) does not cross 3.0σ so NOTHING is emitted (the boundary
// holds even at a threshold stricter than today's default); (B) at 2.0σ the drifter IS recorded
// idiosyncratic and a common-mode shift is NOT flagged idiosyncratic. In BOTH: routing_patterns is
// byte-identical after RunOnce (Keel never mutates the corpus) and no row names a counterparty.
func TestBreach_QueryOnlyAndNoRawCounterparty(t *testing.T) {
	ctx := context.Background()
	pool := keelTestPool(t)
	const provider = "keelt_noact"
	cleanup(t, pool, provider)
	self := map[string]bool{"a": true, "b": true, "c": true, "d": true, "e": true, "f": true}

	// unit "m" — IDIOSYNCRATIC: 6 opted-in ws, w1 all 0.8; w2 "a" drifts to 0.1, rest hold.
	all := []string{"a", "b", "c", "d", "e", "f"}
	for _, ws := range all {
		seedPattern(t, pool, ws, provider, "m", 0.8, true, w1)
	}
	seedPattern(t, pool, "a", provider, "m", 0.1, true, w2)
	for _, ws := range all[1:] {
		seedPattern(t, pool, ws, provider, "m", 0.8, true, w2)
	}
	// unit "c" — COMMON-MODE: a persistent outlier "f", then ALL shift by −0.3 (e.g. the model degraded).
	base := map[string]float64{"a": 0.5, "b": 0.5, "c": 0.5, "d": 0.5, "e": 0.5, "f": 0.9}
	for _, ws := range all {
		seedPattern(t, pool, ws, provider, "c", base[ws], true, w1)
		seedPattern(t, pool, ws, provider, "c", base[ws]-0.3, true, w2)
	}

	corpusCount := func() int {
		var n int
		// Check the Scan error: a silently-errored count returns 0, and the "corpus unchanged after RunOnce"
		// assertion below would then pass for the WRONG reason (0 == 0). Fail loudly instead.
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_patterns WHERE provider_used=$1`, provider).Scan(&n); err != nil {
			t.Fatalf("corpusCount: %v", err)
		}
		return n
	}
	before := corpusCount()
	// lookback covers the 2026-01-02 seeds (past timestamps) from time.Now().
	runAt := func(sigma float64) {
		sw := keel.NewSweep(keel.NewReader(pool), keel.NewFindingsWriter(pool),
			keel.Config{MinWorkspaces: 3, DeviationSigma: sigma}, 3600, 400*24*time.Hour)
		if _, err := sw.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce(sigma=%v): %v", sigma, err)
		}
		if after := corpusCount(); after != before {
			t.Errorf("RunOnce(sigma=%v) mutated the corpus: before=%d after=%d", sigma, before, after)
		}
	}

	// (A) EXPLICIT 3.0σ (pinned literal — independent of DefaultConfig, now 2.0): boundary holds; the
	// drifter (~2.2σ) does NOT cross 3.0σ ⇒ NOTHING emitted. Proves the boundary at a threshold stricter
	// than today's default, so the suite never depends on a config the default wouldn't emit under.
	runAt(3.0)
	if tot, _ := countFindings(t, pool, provider+"/m", self); tot != 0 {
		t.Errorf("at an explicit 3.0σ the drifter must NOT emit (below threshold), got %d findings", tot)
	}
	if tot, _ := countFindings(t, pool, provider+"/c", self); tot != 0 {
		t.Errorf("at an explicit 3.0σ the common-mode outlier must NOT emit, got %d findings", tot)
	}

	// (B) EXPLICIT 2.0σ (pinned literal — also today's default): the drifter IS idiosyncratic; the
	// common-mode cohort-wide shift is NOT flagged idiosyncratic.
	runAt(2.0)
	if _, idio := countFindings(t, pool, provider+"/m", self); idio < 1 {
		t.Errorf("at 2.0σ the drifter 'a' must be recorded idiosyncratic, got %d idiosyncratic findings", idio)
	}
	if _, idio := countFindings(t, pool, provider+"/c", self); idio != 0 {
		t.Errorf("at 2.0σ a common-mode cohort-wide shift must NOT be flagged idiosyncratic, got %d", idio)
	}
}

// BREACH 5 — APPEND-ONLY DEDUP: re-recording the same finding is a no-op (ON CONFLICT identity_key).
func TestBreach_AppendOnlyDedup(t *testing.T) {
	ctx := context.Background()
	pool := keelTestPool(t)
	const provider = "keelt_dedup"
	cleanup(t, pool, provider)
	w := keel.NewFindingsWriter(pool)
	f := keel.Finding{WorkspaceID: "wsX", Unit: provider + "/m", Window: 42, DeviationSigma: -3.1, Attribution: keel.AttributionIdiosyncratic, CohortN: 4}
	ins1, err := w.Record(ctx, f, map[string]any{"cohort_mean": 0.7})
	if err != nil || !ins1 {
		t.Fatalf("first Record inserted=%v err=%v, want true/nil", ins1, err)
	}
	ins2, err := w.Record(ctx, f, map[string]any{"cohort_mean": 0.7})
	if err != nil {
		t.Fatalf("second Record err=%v", err)
	}
	if ins2 {
		t.Errorf("a re-recorded finding must dedup (ON CONFLICT identity_key), got inserted=true")
	}
	var n int
	// Check the Scan error: a silently-errored count returns 0, which would fail this assert for the wrong
	// reason (or, worse, mask a real dedup regression). Fail loudly on the query error.
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM keel_findings WHERE unit=$1`, provider+"/m").Scan(&n); err != nil {
		t.Fatalf("count keel_findings: %v", err)
	}
	if n != 1 {
		t.Errorf("append-only: exactly 1 row after a double-record, got %d", n)
	}
}

// BREACH (K3) — HARDENED findings are SQL-DISTINGUISHABLE from ordinary (H5 may read ONLY mode='hardened',
// so it can never slash on an ordinary contaminable-mean finding). Also proves hardened append-only dedup.
func TestBreach_HardenedFindingsSQLDistinguishable(t *testing.T) {
	ctx := context.Background()
	pool := keelTestPool(t)
	const provider = "keelt_hmode"
	cleanup(t, pool, provider)
	unit := provider + "/m"
	w := keel.NewFindingsWriter(pool)
	f := keel.Finding{WorkspaceID: "wsH", Unit: unit, Window: 7, DeviationSigma: -4.2, Attribution: keel.AttributionIdiosyncratic, CohortN: 12, CohortMean: 0.8, CohortStdDev: 0.05}
	// Same (ws,unit,window) → ordinary + hardened have DIFFERENT identity_keys, so both insert.
	if ins, err := w.Record(ctx, f, map[string]any{"cohort_mean": 0.8}); err != nil || !ins {
		t.Fatalf("ordinary Record: ins=%v err=%v", ins, err)
	}
	if ins, err := w.RecordHardened(ctx, f, map[string]any{"robust_median": 0.8}); err != nil || !ins {
		t.Fatalf("hardened Record: ins=%v err=%v", ins, err)
	}
	if ins, err := w.RecordHardened(ctx, f, map[string]any{"robust_median": 0.8}); err != nil || ins {
		t.Errorf("second RecordHardened must dedup append-only; ins=%v err=%v", ins, err)
	}
	// SQL-level filter: exactly one row per mode.
	var ord, hard int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM keel_findings WHERE unit=$1 AND mode='ordinary'`, unit).Scan(&ord); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM keel_findings WHERE unit=$1 AND mode='hardened'`, unit).Scan(&hard); err != nil {
		t.Fatal(err)
	}
	if ord != 1 || hard != 1 {
		t.Errorf("SQL-distinguishable: want 1 ordinary + 1 hardened, got ord=%d hard=%d", ord, hard)
	}
	// ListHardenedFindings returns ONLY hardened rows — an ordinary finding can never leak through it.
	hl, err := keel.NewReader(pool).ListHardenedFindings(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	seenH := false
	for _, lf := range hl {
		if lf.Unit != unit {
			continue
		}
		seenH = true
		if lf.WorkspaceID != "wsH" {
			t.Errorf("hardened read named a non-self workspace %q", lf.WorkspaceID)
		}
	}
	if !seenH {
		t.Error("ListHardenedFindings must return the hardened row for this unit")
	}
}

// BREACH (K3) — end-to-end hardened SWEEP: a drifter robustly + persistently below a money-grade cohort
// yields a mode='hardened' row; the tenancy boundary holds (self workspace_id only, aggregates only).
func TestBreach_HardenedSweep_EndToEnd(t *testing.T) {
	ctx := context.Background()
	pool := keelTestPool(t)
	const provider = "keelt_hsweep"
	cleanup(t, pool, provider)
	honest := []struct {
		ws string
		q  float64
	}{{"o1", 0.82}, {"o2", 0.84}, {"o3", 0.85}, {"o4", 0.86}, {"o5", 0.87}, {"o6", 0.88}}
	for _, at := range []time.Time{w1, w2} { // 2 windows (persistence)
		seedPattern(t, pool, "D", provider, "m", 0.10, true, at) // the drifter, far below
		for _, h := range honest {
			seedPattern(t, pool, h.ws, provider, "m", h.q, true, at)
		}
	}
	sw := keel.NewSweep(keel.NewReader(pool), keel.NewFindingsWriter(pool), keel.DefaultConfig(), 3600, 400*24*time.Hour)
	sw.EnableHardened(keel.HardenedConfig{MoneyCohortFloor: 5, MinSamples: 1, PersistenceWindows: 2, HardenedSigma: 3.0})
	if _, err := sw.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	unit := provider + "/m"
	// a hardened row for the drifter exists and is SQL-filterable.
	var dHard int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM keel_findings WHERE unit=$1 AND mode='hardened' AND workspace_id='D'`, unit).Scan(&dHard); err != nil {
		t.Fatal(err)
	}
	if dHard < 1 {
		t.Errorf("hardened sweep must record a mode='hardened' finding for drifter D; got %d", dHard)
	}
	// tenancy: every hardened row names only a SELF (seeded) workspace — never a raw counterparty.
	self := map[string]bool{"D": true, "o1": true, "o2": true, "o3": true, "o4": true, "o5": true, "o6": true}
	rows, err := pool.Query(ctx, `SELECT workspace_id, cohort_n FROM keel_findings WHERE unit=$1 AND mode='hardened'`, unit)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var ws string
		var cohortN int
		if err := rows.Scan(&ws, &cohortN); err != nil {
			t.Fatal(err)
		}
		if !self[ws] {
			t.Errorf("hardened finding names non-self workspace %q — counterparty leak", ws)
		}
		if cohortN < 5 { // leave-one-out cohort >= MoneyCohortFloor (>= privacy floor) — no inversion
			t.Errorf("hardened cohort_n=%d below the money floor — inversion risk", cohortN)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
