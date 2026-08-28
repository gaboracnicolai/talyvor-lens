package poolshadow_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/poolshadow"
	"github.com/talyvor/lens/migrations"
)

// THE DEFECT THIS PACKAGE EXISTS FOR, stated as the test that would have caught the design:
//
// W4.9 asks for a log of what WOULD have pooled, so the pooled hit rate becomes measurable on real
// traffic with pooling OFF. The obvious implementation — on a private miss, do the pooled LOOKUP
// anyway and record the would-be hit — CAN ONLY EVER RECORD ZERO: both pooled read surfaces read a
// keyspace that is only written under cache_pooling.PoolabilityGate.Participant, and the write side
// is gated on the SAME predicate. With pooling off the pooled keyspace is empty, so a lookup-based
// shadow log reports "0 would have pooled" forever and the next reader concludes pooling is worthless.
//
// So this log is written from the WRITE side and the hit rate is computed OFFLINE from repeats.
// TestShadowLog_WriteSideCountsWhatALookupCannot is the two-sided proof: the same traffic that makes
// a lookup-based shadow log report zero makes this one report one.

func realPGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_poolshadow_%d", time.Now().UnixNano())
	adminConn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	if _, err := adminConn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = adminConn.Close(ctx)
		t.Fatalf("create %s: %v", name, err)
	}
	_ = adminConn.Close(ctx)

	dsn := swapDBName(t, admin, name)
	migConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	if _, err := dbmigrate.Run(ctx, migConn, migrations.FS); err != nil {
		_ = migConn.Close(ctx)
		t.Fatalf("apply migrations: %v", err)
	}
	_ = migConn.Close(ctx)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func swapDBName(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// obs is a shorthand for one write-side observation at a chosen instant.
func obs(t *testing.T, r *poolshadow.Recorder, at time.Time, wsID, provider, model, prompt string) {
	t.Helper()
	o := poolshadow.Observe(wsID, provider, model, cache.PooledPromptKey(prompt), prompt, false)
	o.At = at
	if err := r.Record(context.Background(), o); err != nil {
		t.Fatalf("record: %v", err)
	}
}

// TestShadowLog_WriteSideCountsWhatALookupCannot — the load-bearing assertion.
//
// Two DIFFERENT workspaces send the SAME prompt to the same provider+model, minutes apart, with
// pooling OFF. Production would have served the second from the pool had pooling been on; today it
// pays upstream twice. The write-side log must report exactly one would-be cross-tenant hit.
func TestShadowLog_WriteSideCountsWhatALookupCannot(t *testing.T) {
	pool := realPGPool(t)
	r := poolshadow.New(pool)
	ctx := context.Background()

	base := time.Now().Add(-time.Hour)
	obs(t, r, base, "wsA", "openai", "gpt-4o", "how do I fix ImportError in python 3.12")
	obs(t, r, base.Add(5*time.Minute), "wsB", "openai", "gpt-4o", "how do I fix ImportError in python 3.12")

	got, err := r.Rate(ctx, base.Add(-time.Minute), 24*time.Hour)
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	if got.Observations != 2 {
		t.Fatalf("observations = %d, want 2 — the population floor: a rate over nothing is not a rate", got.Observations)
	}
	if got.CrossTenantExactHits != 1 {
		t.Fatalf("cross-tenant would-be exact hits = %d, want 1 — the write-side log has gone blind, "+
			"which is the SAME zero a lookup-based shadow log reports by construction",
			got.CrossTenantExactHits)
	}
}

// TestShadowLog_SameWorkspaceRepeatIsNotAPoolHit — the overstatement guard.
//
// A repeat from the SAME workspace would have been served by the workspace-PRIVATE cache; pooling
// buys nothing there. Counting it would inflate the pool's measured value with savings the private
// cache already delivers, which is precisely the number this log exists to get right.
func TestShadowLog_SameWorkspaceRepeatIsNotAPoolHit(t *testing.T) {
	pool := realPGPool(t)
	r := poolshadow.New(pool)
	base := time.Now().Add(-time.Hour)
	obs(t, r, base, "wsA", "openai", "gpt-4o", "same tenant asks twice")
	obs(t, r, base.Add(time.Minute), "wsA", "openai", "gpt-4o", "same tenant asks twice")

	got, err := r.Rate(context.Background(), base.Add(-time.Minute), 24*time.Hour)
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	if got.Observations != 2 {
		t.Fatalf("observations = %d, want 2", got.Observations)
	}
	if got.CrossTenantExactHits != 0 {
		t.Fatalf("cross-tenant would-be exact hits = %d, want 0 — a same-workspace repeat is a PRIVATE "+
			"cache hit and must never be attributed to the pool", got.CrossTenantExactHits)
	}
}

// TestShadowLog_WindowIsTheCacheTTLAndIsHonoured — a pooled entry that has aged out cannot be hit.
func TestShadowLog_WindowIsTheCacheTTLAndIsHonoured(t *testing.T) {
	pool := realPGPool(t)
	r := poolshadow.New(pool)
	base := time.Now().Add(-72 * time.Hour)
	obs(t, r, base, "wsA", "openai", "gpt-4o", "aged out by the ttl")
	obs(t, r, base.Add(48*time.Hour), "wsB", "openai", "gpt-4o", "aged out by the ttl")

	// 24h TTL: the second request arrives 48h later, long after the entry expired.
	got, err := r.Rate(context.Background(), base.Add(-time.Minute), 24*time.Hour)
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	if got.CrossTenantExactHits != 0 {
		t.Fatalf("cross-tenant would-be exact hits = %d, want 0 — the pooled entry had expired, so "+
			"counting it credits the pool with a hit Redis could not have served", got.CrossTenantExactHits)
	}
	// The positive control on the same data: widen the window past the gap and the hit appears.
	// Without this, "0" is indistinguishable from a query that finds nothing at all.
	wide, err := r.Rate(context.Background(), base.Add(-time.Minute), 72*time.Hour)
	if err != nil {
		t.Fatalf("rate wide: %v", err)
	}
	if wide.CrossTenantExactHits != 1 {
		t.Fatalf("CONTROL FAILED: with a 72h window the same pair yields %d hits, want 1 — the zero "+
			"above was the query being blind, not the TTL being honoured", wide.CrossTenantExactHits)
	}
}

// TestShadowLog_DoesNotCollideAcrossProviderOrModel — the pooled key is per (provider, model).
func TestShadowLog_DoesNotCollideAcrossProviderOrModel(t *testing.T) {
	pool := realPGPool(t)
	r := poolshadow.New(pool)
	base := time.Now().Add(-time.Hour)
	obs(t, r, base, "wsA", "openai", "gpt-4o", "identical text, different model")
	obs(t, r, base.Add(time.Minute), "wsB", "openai", "gpt-4o-mini", "identical text, different model")
	obs(t, r, base.Add(2*time.Minute), "wsB", "anthropic", "gpt-4o", "identical text, different model")

	got, err := r.Rate(context.Background(), base.Add(-time.Minute), 24*time.Hour)
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	if got.CrossTenantExactHits != 0 {
		t.Fatalf("cross-tenant would-be exact hits = %d, want 0 — a pooled entry is keyed on "+
			"(provider, model, prompt); crossing either would serve an answer from a model the "+
			"caller did not ask for", got.CrossTenantExactHits)
	}
}

// TestShadowLog_EntityGateCeilingIsReportedSeparately.
//
// cmd/hitrate's finding is that the SQL's `discriminators = $6` equality, not the cosine threshold,
// is what bounds the semantic pool — and that ceiling is threshold-independent. The shadow log
// reports it as its own figure, over real traffic, and NEVER as a hit: a matching entity gate is a
// necessary condition for a semantic serve, not a sufficient one.
func TestShadowLog_EntityGateCeilingIsReportedSeparately(t *testing.T) {
	pool := realPGPool(t)
	r := poolshadow.New(pool)
	base := time.Now().Add(-time.Hour)
	// Same discriminators (python 3.12 / ImportError), different wording — no exact hit possible.
	obs(t, r, base, "wsA", "openai", "gpt-4o", "ImportError on python 3.12, what do I do")
	obs(t, r, base.Add(time.Minute), "wsB", "openai", "gpt-4o", "getting an ImportError with python 3.12 — help")

	got, err := r.Rate(context.Background(), base.Add(-time.Minute), 24*time.Hour)
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	if got.CrossTenantExactHits != 0 {
		t.Fatalf("exact hits = %d, want 0 — the two prompts are not byte-identical", got.CrossTenantExactHits)
	}
	if got.CrossTenantEntityGateCeiling != 1 {
		t.Fatalf("entity-gate ceiling = %d, want 1 — the two prompts share a canonical discriminator "+
			"set, so the SQL equality that bounds the semantic pool WOULD have admitted this pair",
			got.CrossTenantEntityGateCeiling)
	}
}
