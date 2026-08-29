package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// background_job_classification_test.go — every background goroutine main.go starts, and whether
// running it on more than one replica is safe.
//
// main.go starts 45 goroutines. 35 are wrapped in `haComps.leader.Run(...)` — exactly one instance
// across the fleet — and the file already documents the other category in as many words ("NOT
// leader-gated: each replica must refresh its OWN cache"). ⚠ SO THE CODEBASE ALREADY KNOWS THERE
// ARE TWO BUCKETS. Nothing checked which bucket a given job was in, and a job added to the wrong
// one is invisible: it works perfectly on one replica and misbehaves only after a scale-out.
//
// ⚠ THE COUNT IS NOT THE FINDING AND WAS NEVER GOING TO BE. Nine of the ten ungated jobs are
// ungated CORRECTLY, and leader-gating any of them would be a bug — a per-replica cache that only
// the leader refreshed would serve every follower stale. Each is classified individually below with
// the property that makes it safe, because a census that reports "10 ungated jobs" as a defect is
// manufacturing one.
//
// THE PROPERTY THAT DECIDES IT, measured per job rather than argued from the schedule:
//
//	IN-PROCESS STATE     the work touches this replica's own memory. MUST be per-replica.
//	IDEMPOTENT           an age predicate or a row lock makes the Nth run affect nothing.
//	PER-REQUEST          started inside an HTTP handler; it belongs to no fleet-wide bucket.
//	SINGLETON            shared state, non-idempotent, or it costs money. Must be leader-gated.

// perReplica is every goroutine deliberately NOT leader-gated, with the measured property that
// makes running it on every replica correct. ⚠ A REASON HERE IS A CLAIM ABOUT CODE, not a label:
// each was read before being written down.
var perReplica = map[string]string{
	"batchRouter.StartPoller": "IN-PROCESS STATE. pollAll iterates r.pending, a map held in this " +
		"replica's memory, so a replica polls only the jobs submitted to it. Nothing to duplicate.",
	"sessionTracker.StartCleanup": "IN-PROCESS STATE. evictStale walks t.sessions under t.mu — an " +
		"in-memory map. Leader-gating it would leak session state on every follower.",
	"statusPage.StartCacher": "IN-PROCESS STATE. Refreshes this replica's own status snapshot; a " +
		"follower that never refreshed would serve a stale status page forever.",
	"l.StartBackground": "READ-ONLY. The learner's loop calls Analyse and logs the top patterns. " +
		"Measured: no INSERT, no UPDATE, no ON CONFLICT anywhere in internal/learner. Duplicate log " +
		"lines across replicas are the intended per-replica observability.",
	"semanticCache.StartSweeper": "IDEMPOTENT. DeleteStale is a single DELETE bounded by " +
		"`created_at < cutoff`. The second replica's sweep on the same tick affects zero rows — an " +
		"age predicate, not a top-N eviction, which is the distinction that matters here.",
	"cpSyncer.Run": "IN-PROCESS STATE. Rebuilds this replica's compression-policy cache; main.go " +
		"says so directly at the reload-interval comment — each replica must refresh its OWN cache.",
	"detector staleness": "PER-REPLICA METRIC. Publishes a gauge so a stalled detector is visible " +
		"between runs. Every replica should report its own liveness.",
	"stranded reservation sweep": "IDEMPOTENT BY ROW LOCK, and this one was checked hardest because " +
		"it MOVES MONEY. ReleaseStrandedReservations reads held ids and then refunds each — a " +
		"read-then-write two concurrent replicas both enter. ReleaseLXCReservation takes the row " +
		"`FOR UPDATE` inside a transaction and returns early when status != 'held', so the second " +
		"refund is a no-op. Its own comment says \"a double-sweep ... is a safe no-op\". Double " +
		"refund is impossible.",
	"localRouterMulti.CheckHealth": "PER-REQUEST. Started inside an HTTP handler.",
	"audit export POST":            "PER-REQUEST. Started inside an HTTP handler.",
}

// perReplicaMatch maps a classification key to the CALL that identifies its goroutine: either the
// callee itself, or — for a closure — a call inside its body.
//
// ⚠ IT MAPPED TO A SUBSTRING OF THE `go` LINE UNTIL #528, AND THREE OF THE TEN WERE CLOSURES
// IDENTIFIED BY THE TEXT AFTER `go func(`. One was a COMMENT. And the "stranded reservation
// sweep" needle reduced, after the matcher split it on "\n" and kept index 0, to `go func() {` —
// which every anonymous goroutine in this file begins with, so any new one silently inherited
// that entry's money-path reason. A call is not a spelling; these are calls.
var perReplicaMatch = map[string]string{
	"batchRouter.StartPoller":      "batchRouter.StartPoller",
	"sessionTracker.StartCleanup":  "sessionTracker.StartCleanup",
	"statusPage.StartCacher":       "statusPage.StartCacher",
	"l.StartBackground":            "l.StartBackground",
	"semanticCache.StartSweeper":   "semanticCache.StartSweeper",
	"cpSyncer.Run":                 "cpSyncer.Run",
	"localRouterMulti.CheckHealth": "localRouterMulti.CheckHealth",
	// The three closures, each identified by the one call that is its whole point.
	"detector staleness":         "patternDetectorHealth.PublishAge",
	"stranded reservation sweep": "dualToken.ReleaseStrandedReservations",
	"audit export POST":          "auditExporter.ExportWebhook",
}

// ⚠ THE GUARD. A goroutine that is neither leader-gated nor classified is one nobody has decided
// about, and the wrong answer is invisible until the fleet grows past one replica.
func TestEveryBackgroundGoroutineIsClassified(t *testing.T) {
	sites, err := scanGoStatements("main.go", []byte(readMainGo(t)))
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var gated, unclassified []string
	for _, g := range sites {
		if g.jobName != "" {
			gated = append(gated, g.jobName)
			continue
		}
		var hits []string
		for key, needle := range perReplicaMatch {
			if g.matches(needle) {
				if perReplica[key] == "" {
					t.Errorf("%s is classified per-replica with no reason", key)
				}
				hits = append(hits, key)
			}
		}
		switch len(hits) {
		case 0:
			what := g.callee
			if what == "" {
				what = "func literal calling " + strings.Join(g.calls, ", ")
			}
			unclassified = append(unclassified, fmt.Sprintf("main.go:%d %s", g.line, what))
		case 1:
			// classified
		default:
			sort.Strings(hits)
			t.Errorf("the goroutine at main.go:%d matches %d classifications (%s) — a goroutine "+
				"that answers to two reasons has been decided about by neither",
				g.line, len(hits), strings.Join(hits, ", "))
		}
	}

	// Non-vacuity: a parse that finds nothing satisfies every check above.
	if len(gated) < 30 {
		t.Fatalf("found %d leader-gated jobs, expected 30+ — the scan is broken, and a broken scan "+
			"reports every job as classified", len(gated))
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Errorf("%d background goroutine(s) are neither leader-gated nor classified:\n  %s\n\n"+
			"    Wrap it in haComps.leader.Run if it touches SHARED state, is not idempotent, or "+
			"COSTS MONEY — on N replicas it runs N times.\n"+
			"    Or add it to perReplica with the measured property that makes running it "+
			"everywhere correct: in-process state, an age predicate, or a row lock. Say which.",
			len(unclassified), strings.Join(unclassified, "\n  "))
	}
	t.Logf("MEASURED: %d leader-gated singletons, %d classified per-replica.", len(gated), len(perReplica))
}

// ⚠ THE ONE THAT WAS IN THE WRONG BUCKET, PINNED BY NAME SO IT CANNOT DRIFT BACK.
//
// The cache warmer re-fires historical prompts at api.openai.com and api.anthropic.com on the
// OPERATOR'S KEYS, hourly. It is the only background job in this binary that spends money at an
// external provider, and it was the only ungated one whose state is SHARED: exactCache is
// cache.NewExactCache(redisClient, …) — Redis, one instance for the fleet — and WarmOne reads and
// writes nothing else. Every byte of its benefit is shared; none of its cost was.
//
// Its dedup is `w.mu` + the `w.warming` map: in-process, so it deduplicates nothing across
// replicas. GetWarmCandidates is a plain `LIMIT 10` with no claim, no lock and no SKIP LOCKED, so
// every replica selects the SAME ten. The Redis Get check saves a replica that starts late; on a
// rolling deploy or a scale-out, replicas boot together and tick together.
//
// ⚠ AND IT IS LATENT, NOT LIVE, WHICH BELONGS IN THE RECORD. Production is a single docker-compose
// instance (W5.1), so N = 1 and nothing is being double-spent today. ha.Leader.Run calls fn
// directly when HA is disabled, so gating it changes nothing for a single replica — which is
// exactly why it was safe to do rather than only report.
func TestTheCacheWarmerIsLeaderGated(t *testing.T) {
	src := []byte(readMainGo(t))
	// ⚠ BOTH HALVES WERE strings.Contains OVER THE RAW SOURCE UNTIL #526, and a closure walked
	// past both: `go func() { cacheWarmer.Start(ctx, …) }()` with the leader.Run line left in a
	// COMMENT satisfied the positive half and dodged the negative one, which looked for the exact
	// text `go cacheWarmer.Start(ctx`. The question is now (a) does the named leader job exist as
	// a real call, and (b) is EVERY cacheWarmer.Start call lexically inside it.
	// Arms: ~/talyvor-queue/w61-operatorkeys-controls-h2r7.py.
	jobs, err := scanLeaderJobs("main.go", src)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	if len(jobs) < 20 {
		t.Fatalf("scanLeaderJobs found only %d leader.Run registrations — the scan is blind", len(jobs))
	}
	gated := false
	for _, j := range jobs {
		if j.name == "cache-warmer" {
			gated = true
		}
	}
	if !gated {
		t.Error("the cache warmer is not leader-gated.\n\n" +
			"    It re-fires historical prompts at OpenAI and Anthropic on the operator's keys, " +
			"hourly, and writes only to the SHARED Redis exact cache. Its dedup (w.mu + w.warming) " +
			"is in-process, and GetWarmCandidates takes a plain LIMIT 10 with no claim — so N " +
			"replicas pick the same ten prompts and pay for them N times.\n" +
			"    Every other money-touching singleton in this file is wrapped in leader.Run, " +
			"including audit-retention, which only deletes rows.")
	}
	loose, err := callsOutsideLeaderJob("main.go", src, "cacheWarmer", "Start", "cache-warmer")
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	if len(loose) > 0 {
		t.Errorf("cacheWarmer.Start is called at main.go line(s) %v OUTSIDE the \"cache-warmer\" "+
			"leader job — the warmer would run on every replica AND on the leader", loose)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
