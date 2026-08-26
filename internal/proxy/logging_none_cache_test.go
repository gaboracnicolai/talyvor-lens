package proxy

// logging_none_cache_test.go — WHAT `logging_policy = none` ACTUALLY STOPS.
//
// ⚠ THE CLAIM UNDER TEST IS THIS REPO'S OWN, IN TWO PLACES.
//
//   proxy.go, beside the observability writes:
//     "Logging policy gates the per-request observability writes. None is the privacy escape
//      hatch — EVERY DB AND NATS SINK IS BYPASSED."
//
//   talyvor-suite apps/web Landing (the shipped marketing page):
//     "Prompts, issues, pages, and spend records sit in your Postgres. Retention is a
//      per-workspace policy you set — including 'log nothing' — not a plan tier."
//
// ⚠ AND THERE IS A DB SINK THAT IS NOT BYPASSED: THE SEMANTIC CACHE. `storeCaches` writes the
// RESPONSE BODY into `prompt_embeddings` — the table this repo's own tenant-data manifest
// describes as "the cached ANSWERS — the most sensitive thing held" — and it is reached on a path
// that never consults the policy:
//
//   · storeCaches TAKES NO logging policy and fetches none. It cannot honour one.
//   · Its four call sites (proxy.go ×3, stream.go ×1) guard on statusCode, PII and a quality
//     score. None of them mentions the policy.
//   · internal/cache does not import internal/workspace at all, so SemanticCache.Set has nothing
//     to consult either.
//
// ⚠ WHAT THIS FILE DOES NOT CLAIM. It does not claim the cache SHOULD be bypassed — that is a
// decision with two defensible answers, set out in docs/retention-none-and-the-semantic-cache.md,
// and it is not a session's to take: bypassing costs `none` workspaces every cache hit, which
// raises their bill. What this file does is make the fact EXECUTABLE, so it is re-measured on
// every CI run instead of resting on a comment that is currently wrong.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/workspace"
)

const noneCacheSchema = "lens_it_nonecache"

func noneCachePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG retention test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = noneCacheSchema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	// The columns SemanticCache.Set actually writes, matching migrations' prompt_embeddings.
	// `embedding` is plain text here rather than vector(1536): this test never reads by
	// similarity, and requiring the pgvector extension inside a throwaway schema would make the
	// test skip on a plain-postgres runner — a skip that would look like a pass.
	if _, err := pool.Exec(context.Background(), `
		DROP SCHEMA IF EXISTS `+noneCacheSchema+` CASCADE;
		CREATE SCHEMA `+noneCacheSchema+`;
		CREATE TABLE `+noneCacheSchema+`.prompt_embeddings (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			provider        TEXT NOT NULL,
			model           TEXT NOT NULL,
			prompt_hash     TEXT NOT NULL UNIQUE,
			embedding       TEXT,
			response        TEXT NOT NULL,
			tokens_saved    INTEGER NOT NULL DEFAULT 0,
			hit_count       INTEGER NOT NULL DEFAULT 0,
			created_at      TIMESTAMPTZ DEFAULT NOW(),
			updated_at      TIMESTAMPTZ DEFAULT NOW(),
			contributor_workspace_id TEXT,
			is_poolable     BOOLEAN NOT NULL DEFAULT false,
			workspace_id    TEXT,
			embedding_model TEXT,
			discriminators  TEXT,
			variant_of      UUID
		);
	`); err != nil {
		t.Fatalf("build fixture schema: %v", err)
	}
	return pool
}

// This test reuses the package's existing fixedEmbedder (semantic_pooling_test.go) rather than
// declaring another: the embedder is not the thing under test — the WRITE is — and a real
// embedding call would add a network dependency and a bill to a question about retention.

// ⚠ THE MEASUREMENT.
func TestMeasured_LoggingNoneStillPersistsTheAnswerToTheSemanticCache(t *testing.T) {
	pool := noneCachePool(t)
	ctx := context.Background()

	const wsID = "ws-logging-none"
	const secret = "the user's private answer, which this workspace asked never to be logged"

	// A REAL workspace.Manager, set to the policy whose whole promise is that nothing is kept.
	wsm := workspace.New(nil)
	if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{
		ID: wsID, Name: wsID, LoggingPolicy: workspace.LoggingNone,
	}); err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	if got := wsm.GetLoggingPolicy(wsID); got != workspace.LoggingNone {
		t.Fatalf("the fixture never reached the policy under test: GetLoggingPolicy = %q, want %q. "+
			"Everything below would then be measuring a DIFFERENT workspace's rules", got, workspace.LoggingNone)
	}

	// A REAL SemanticCache over the real upsert SQL, and the REAL storeCaches.
	p := &Proxy{
		semantic: cache.NewSemanticCache(pool, fixedEmbedder{}, 0.9, 0),
		embedder: fixedEmbedder{},
		// ⚠ THE MANAGER IS WIRED IN, AND IT HAS TO BE. The first version of this fixture left
		// workspaceManager nil and registered the policy on a manager the Proxy never saw — so
		// loggingPolicyFor returned the METADATA default and the workspace's `none` was
		// decorative. Control F1 caught it: gating storeCaches on the policy left this test GREEN,
		// because the policy could not be reached from the code under test. With the manager
		// attached, p.loggingPolicyFor(wsID) genuinely returns `none` and the write happening
		// anyway is a fact about the product rather than about the fixture.
		workspaceManager: wsm,
		// exact and poolGate stay nil: the exact cache is Redis (not the sink in question) and a
		// nil gate writes no pooled copy, which is the DEFAULT posture. This isolates the
		// workspace-PRIVATE semantic write — the one with no opt-in of any kind.
	}
	if got := p.loggingPolicyFor(wsID); got != workspace.LoggingNone {
		t.Fatalf("the PROXY resolves this workspace to %q, not %q — the policy is not reachable "+
			"from the code under test, so nothing below would be measuring retention at all", got, workspace.LoggingNone)
	}
	p.storeCaches(ctx, "openai", "gpt-4o", wsID+":"+"what is my diagnosis", "what is my diagnosis", wsID, []byte(secret))

	var rows int
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(max(response), '') FROM prompt_embeddings WHERE workspace_id = $1`,
		wsID).Scan(&rows, &stored); err != nil {
		t.Fatalf("read back: %v", err)
	}

	if rows == 0 {
		t.Fatalf("no row was written for a %q workspace.\n\n"+
			"⚠ IF YOU ARE READING THIS BECAUSE CI WENT RED, THAT IS THE FIX LANDING, NOT A BREAKAGE: "+
			"the semantic-cache write now honours the logging policy. Delete this test and the "+
			"decision section it points at in docs/retention-none-and-the-semantic-cache.md.",
			workspace.LoggingNone)
	}
	if !strings.Contains(stored, secret) {
		t.Fatalf("a row exists but does not hold the answer (%q) — then this test is measuring "+
			"something other than content retention", stored)
	}

	t.Logf("MEASURED: workspace %q is on logging_policy=%q, and its answer is in "+
		"prompt_embeddings.response verbatim (%d row). storeCaches takes no policy and consults "+
		"none; internal/cache does not import internal/workspace at all.",
		wsID, workspace.LoggingNone, rows)
}

// ─── the census: nothing on the path consults the policy ───────────────────

// policyWindow is how many lines above a call site the census searches for a guard.
//
// ⚠ IT IS DELIBERATELY GENEROUS. A window too small would find no guard and report every call
// site unguarded — a scary-looking failure with an innocent product. The two-sided control below
// is what proves 40 lines is enough to FIND a guard when one is there.
const policyWindow = 40

func policyMentioned(lines []string, at int) bool {
	from := at - policyWindow
	if from < 0 {
		from = 0
	}
	for _, l := range lines[from:at] {
		if strings.Contains(l, "loggingPolicy") || strings.Contains(l, "LoggingNone") ||
			strings.Contains(l, "loggingPolicyFor") {
			return true
		}
	}
	return false
}

// TestCensus_NoStoreCachesCallSiteConsultsTheLoggingPolicy reads the product's own source.
//
// ⚠ AND IT IS TWO-SIDED. The half that matters is the CONTROL: the same window, run over the
// recordTokenEvent write — a sink that IS policy-gated — must FIND the guard. Without that, "no
// guard found" would be indistinguishable from "this census cannot find guards", which is the
// shape of a check that reports a defect it invented.
func TestCensus_NoStoreCachesCallSiteConsultsTheLoggingPolicy(t *testing.T) {
	var lines []string
	var callSites []int
	var files []string
	for _, name := range []string{"proxy.go", "stream.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		fileLines := strings.Split(string(src), "\n")
		base := len(lines)
		lines = append(lines, fileLines...)
		for i, l := range fileLines {
			if strings.Contains(l, "storeCaches(") && !strings.Contains(l, "func (p *Proxy) storeCaches") {
				callSites = append(callSites, base+i)
				files = append(files, name)
			}
		}
	}

	// A floor on the population: if the scan finds nothing, every assertion below is vacuous.
	if len(callSites) < 4 {
		t.Fatalf("found %d storeCaches call sites, expected at least 4 — the scan has gone blind "+
			"(a rename, a move, or a helper indirection), and a census over nothing asserts nothing",
			len(callSites))
	}

	var guarded []string
	for i, at := range callSites {
		if policyMentioned(lines, at) {
			guarded = append(guarded, files[i]+":"+strings.TrimSpace(lines[at]))
		}
	}
	if len(guarded) > 0 {
		t.Fatalf("a storeCaches call site now mentions the logging policy within %d lines:\n  %s\n\n"+
			"⚠ IF THIS IS THE FIX LANDING, THAT IS GOOD NEWS: delete this test and the decision "+
			"section it points at in docs/retention-none-and-the-semantic-cache.md.",
			policyWindow, strings.Join(guarded, "\n  "))
	}

	// ⚠ THE CONTROL. recordTokenEvent's write IS policy-gated (proxy.go strips prompt_text under
	// metadata and skips the row under none). The same window must find that guard, or the
	// assertion above is measuring the searcher rather than the source.
	src, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("read proxy.go: %v", err)
	}
	proxyLines := strings.Split(string(src), "\n")
	found := false
	for i, l := range proxyLines {
		if strings.Contains(l, "p.recordTokenEvent(") && policyMentioned(proxyLines, i) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("the CONTROL failed: no recordTokenEvent call site has a logging-policy mention "+
			"within %d lines. Either the policy gate on token_events has been removed — which is a "+
			"far bigger finding than this file's — or this census cannot detect a guard at all, in "+
			"which case its verdict above is worthless", policyWindow)
	}
}

// TestCensus_StoreCachesItselfCannotHonourAPolicy — the function neither receives one nor fetches
// one, so no call site could delegate the decision to it.
func TestCensus_StoreCachesItselfCannotHonourAPolicy(t *testing.T) {
	src, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("read proxy.go: %v", err)
	}
	text := string(src)
	start := strings.Index(text, "func (p *Proxy) storeCaches(")
	if start < 0 {
		t.Fatal("storeCaches is gone — re-anchor this census")
	}
	end := strings.Index(text[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of storeCaches")
	}
	body := text[start : start+end]
	for _, needle := range []string{"loggingPolicy", "LoggingNone", "loggingPolicyFor"} {
		if strings.Contains(body, needle) {
			t.Fatalf("storeCaches now mentions %q — if it honours the policy, this file's finding "+
				"is closed: delete it and the decision section it points at", needle)
		}
	}
	// Two-sided again: the signature really is the one being described.
	if !strings.Contains(body, "wsID string") {
		t.Fatal("storeCaches no longer takes a wsID — re-read the function before trusting the " +
			"assertion above, because the shape it describes has changed")
	}
}
