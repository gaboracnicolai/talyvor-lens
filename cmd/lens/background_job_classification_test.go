package main

import (
	"regexp"
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

// leaderGated matches the wrapper that makes a job a fleet-wide singleton.
var leaderGated = regexp.MustCompile(`^\s*go haComps\.leader\.Run\(ctx, "([a-z0-9-]+)"`)

// anyGoroutine matches every `go` statement in the file.
var anyGoroutine = regexp.MustCompile(`^\s*go `)

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

// perReplicaMatch maps a classification key to the substring identifying its `go` line.
var perReplicaMatch = map[string]string{
	"batchRouter.StartPoller":      "go batchRouter.StartPoller(",
	"sessionTracker.StartCleanup":  "go sessionTracker.StartCleanup(",
	"statusPage.StartCacher":       "go statusPage.StartCacher(",
	"l.StartBackground":            "go l.StartBackground(",
	"semanticCache.StartSweeper":   "go semanticCache.StartSweeper(",
	"cpSyncer.Run":                 "go cpSyncer.Run(",
	"detector staleness":           "go func() { // publish detector staleness",
	"stranded reservation sweep":   "go func() {\n\t\t\tt := time.NewTicker(2 * time.Minute)",
	"localRouterMulti.CheckHealth": "go localRouterMulti.CheckHealth(",
	"audit export POST":            "go func(filter audit.ExportFilter, url string) {",
}

// ⚠ THE GUARD. A goroutine that is neither leader-gated nor classified is one nobody has decided
// about, and the wrong answer is invisible until the fleet grows past one replica.
func TestEveryBackgroundGoroutineIsClassified(t *testing.T) {
	src := readMainGo(t)
	lines := strings.Split(src, "\n")

	var gated, unclassified []string
	for i, line := range lines {
		if !anyGoroutine.MatchString(line) {
			continue
		}
		if m := leaderGated.FindStringSubmatch(line); m != nil {
			gated = append(gated, m[1])
			continue
		}
		var matched bool
		for key, needle := range perReplicaMatch {
			probe := line
			if strings.Contains(needle, "\n") {
				probe = strings.Join(lines[i:min(i+2, len(lines))], "\n")
			}
			if strings.Contains(probe, strings.SplitN(needle, "\n", 2)[0]) {
				if perReplica[key] == "" {
					t.Errorf("%s is classified per-replica with no reason", key)
				}
				matched = true
				break
			}
		}
		if !matched {
			unclassified = append(unclassified, strings.TrimSpace(line))
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
	src := readMainGo(t)
	const want = `go haComps.leader.Run(ctx, "cache-warmer"`
	if !strings.Contains(src, want) {
		t.Errorf("the cache warmer is not leader-gated.\n\n" +
			"    It re-fires historical prompts at OpenAI and Anthropic on the operator's keys, " +
			"hourly, and writes only to the SHARED Redis exact cache. Its dedup (w.mu + w.warming) " +
			"is in-process, and GetWarmCandidates takes a plain LIMIT 10 with no claim — so N " +
			"replicas pick the same ten prompts and pay for them N times.\n" +
			"    Every other money-touching singleton in this file is wrapped in leader.Run, " +
			"including audit-retention, which only deletes rows.")
	}
	if strings.Contains(src, "go cacheWarmer.Start(ctx") {
		t.Error("the ungated `go cacheWarmer.Start(ctx, …)` call is back alongside the gated one — " +
			"the warmer would run on every replica AND on the leader")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
