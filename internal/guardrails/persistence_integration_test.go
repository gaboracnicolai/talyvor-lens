package guardrails

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
)

// guardrailTestPool builds a real-PG pool + a fresh guardrail_policies table
// mirroring migration 0014. Skips when LENS_TEST_DATABASE_URL is unset.
func guardrailTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG guardrails persistence test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS guardrail_policies`,
		`CREATE TABLE guardrail_policies (
			workspace_id TEXT PRIMARY KEY,
			policy       JSONB NOT NULL,
			created_at   TIMESTAMPTZ DEFAULT NOW(),
			updated_at   TIMESTAMPTZ DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func newStoreEngine(pool *pgxpool.Pool) *Engine {
	e := New(pii.New(), injection.New(injection.DefaultPolicy()))
	e.SetStore(pool)
	return e
}

func fullPolicy(wsID string) GuardrailPolicy {
	return GuardrailPolicy{
		WorkspaceID:      wsID,
		EnablePII:        true,
		EnableInjection:  true,
		EnableTopics:     true,
		BlockedTopics:    []string{"weapons", "malware"},
		EnableWordFilter: true,
		BlockedWords:     []string{"secret", "classified"},
		PIIAction:        ActionBlock, // STRICTER than the default Redact
		InjectionAction:  ActionBlock,
		CustomRules:      []CustomRule{{ID: "r1", Name: "rule1", Pattern: "evil.*", Action: ActionBlock, Message: "blocked"}},
		// output stage (Upgrade 13) — all populated to prove JSONB carries them.
		OutputPIIAction:       ActionRedact,
		OutputValidateJSON:    true,
		OutputMaxLength:       1000,
		OutputMustMatch:       "^ok",
		OutputMustNotMatch:    "secret",
		OutputValidationBlock: true,
		BufferStreamForOutput: true,
	}
}

// TestPersist_RoundTrip_FieldIdentical — SetPolicy persists via dbjson; a direct
// SELECT + unmarshal reads back EVERY field identically (lists, custom rules,
// output fields). Proves the #133 dbjson write + JSON read are lossless.
func TestPersist_RoundTrip_FieldIdentical(t *testing.T) {
	pool := guardrailTestPool(t)
	ctx := context.Background()
	e := newStoreEngine(pool)
	want := fullPolicy("ws-full")
	if err := e.SetPolicy(ctx, "ws-full", want); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	var doc []byte
	if err := pool.QueryRow(ctx, `SELECT policy FROM guardrail_policies WHERE workspace_id=$1`, "ws-full").Scan(&doc); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	var got GuardrailPolicy
	if err := json.Unmarshal(doc, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip not field-identical:\n got=%+v\nwant=%+v", got, want)
	}
}

// TestPersist_SurvivesRestart — THE headline: a custom policy set on one engine is
// present in a FRESH engine (a restarted process) after Load. Fixes the
// restart-loss bug.
func TestPersist_SurvivesRestart(t *testing.T) {
	pool := guardrailTestPool(t)
	ctx := context.Background()

	e1 := newStoreEngine(pool)
	if err := e1.SetPolicy(ctx, "ws-r", fullPolicy("ws-r")); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	e2 := newStoreEngine(pool) // a restarted process
	if err := e2.Load(ctx); err != nil {
		t.Fatalf("e2 Load: %v", err)
	}
	got := e2.GetPolicy("ws-r")
	if got.PIIAction != ActionBlock || len(got.BlockedTopics) != 2 || len(got.CustomRules) != 1 || !got.OutputValidateJSON {
		t.Errorf("restart did NOT preserve the custom policy: %+v", got)
	}
}

// TestPersist_PropagatesAcrossInstances — A sets, B is stale until B.Reload.
func TestPersist_PropagatesAcrossInstances(t *testing.T) {
	pool := guardrailTestPool(t)
	ctx := context.Background()
	eA, eB := newStoreEngine(pool), newStoreEngine(pool)
	if err := eB.Load(ctx); err != nil {
		t.Fatalf("B Load: %v", err)
	}
	if eB.GetPolicy("ws-p").PIIAction == ActionBlock {
		t.Fatal("precondition: B should serve default (Redact) before A sets a policy")
	}
	if err := eA.SetPolicy(ctx, "ws-p", fullPolicy("ws-p")); err != nil {
		t.Fatalf("A SetPolicy: %v", err)
	}
	if eB.GetPolicy("ws-p").PIIAction == ActionBlock {
		t.Error("staleness precondition: B should still serve default before reload")
	}
	if err := eB.Reload(ctx); err != nil {
		t.Fatalf("B Reload: %v", err)
	}
	if eB.GetPolicy("ws-p").PIIAction != ActionBlock {
		t.Error("after Reload, B must serve A's tightening (PIIAction=Block)")
	}
}

// TestPersist_DeleteThenReload_RevertsToDefault — DeletePolicy removes the row; a
// reload leaves the workspace at default (not resurrected).
func TestPersist_DeleteThenReload_RevertsToDefault(t *testing.T) {
	pool := guardrailTestPool(t)
	ctx := context.Background()
	eA, eB := newStoreEngine(pool), newStoreEngine(pool)
	if err := eA.SetPolicy(ctx, "ws-d", fullPolicy("ws-d")); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if err := eB.Load(ctx); err != nil || eB.GetPolicy("ws-d").PIIAction != ActionBlock {
		t.Fatalf("precondition: B must have the custom policy; err=%v", err)
	}
	if err := eA.DeletePolicy(ctx, "ws-d"); err != nil {
		t.Fatalf("DeletePolicy: %v", err)
	}
	if err := eB.Reload(ctx); err != nil {
		t.Fatalf("B Reload: %v", err)
	}
	if eB.GetPolicy("ws-d").PIIAction != ActionRedact { // default
		t.Error("after delete+reload, B must revert to default (Redact), not resurrect the custom policy")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM guardrail_policies WHERE workspace_id=$1`, "ws-d").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("DeletePolicy must remove the DB row; count=%d", n)
	}
}

// TestPersist_CorruptRow_Startup_FailsWholeBuild_Degraded — (2)+(3): a single
// unparseable row at STARTUP fails the whole build (no skip) → empty map → default
// for all + Degraded(). The bad workspace is NOT silently downgraded in isolation;
// the degrade is loud + flagged.
func TestPersist_CorruptRow_Startup_FailsWholeBuild_Degraded(t *testing.T) {
	pool := guardrailTestPool(t)
	ctx := context.Background()
	// valid JSON, invalid for the struct (enable_pii expects bool) → unmarshal fails.
	if _, err := pool.Exec(ctx,
		`INSERT INTO guardrail_policies (workspace_id, policy) VALUES ($1, $2::jsonb)`,
		"ws-bad", `{"enable_pii":"not-a-bool"}`); err != nil {
		t.Fatalf("seed corrupt row: %v", err)
	}
	e := newStoreEngine(pool)
	if err := e.Load(ctx); err == nil {
		t.Fatal("Load must FAIL on a corrupt row (whole build fails, no skip)")
	}
	if !e.Degraded() {
		t.Error("startup corrupt-row failure must set Degraded()")
	}
	// default-on-miss is the locked-down baseline (PII redact + injection block ON).
	p := e.GetPolicy("any")
	if !p.EnablePII || p.PIIAction != ActionRedact || !p.EnableInjection || p.InjectionAction != ActionBlock {
		t.Errorf("degraded must serve the locked-down default; got %+v", p)
	}
}

// TestPersist_CorruptRow_Reload_RetainsLastGood_NotDegraded — (1)+(2): a corrupt
// row appearing on RELOAD fails the build but the last-good map (incl. a stricter
// tightening) survives, and Degraded() stays FALSE (we're serving real policies).
func TestPersist_CorruptRow_Reload_RetainsLastGood_NotDegraded(t *testing.T) {
	pool := guardrailTestPool(t)
	ctx := context.Background()
	e := newStoreEngine(pool)
	if err := e.SetPolicy(ctx, "ws-keep", fullPolicy("ws-keep")); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if err := e.Load(ctx); err != nil { // first good load → loaded=true
		t.Fatalf("initial Load: %v", err)
	}
	// a corrupt row appears.
	if _, err := pool.Exec(ctx,
		`INSERT INTO guardrail_policies (workspace_id, policy) VALUES ($1, $2::jsonb)`,
		"ws-bad", `{"enable_injection":42}`); err != nil {
		t.Fatalf("seed corrupt row: %v", err)
	}
	if err := e.Reload(ctx); err == nil {
		t.Fatal("Reload must fail on the corrupt row")
	}
	if e.Degraded() {
		t.Error("reload failure over a good map must NOT be degraded (serving last-good real policies)")
	}
	if got := e.GetPolicy("ws-keep"); got.PIIAction != ActionBlock || len(got.CustomRules) != 1 {
		t.Errorf("reload failure must RETAIN the last-good (stricter) policy, not downgrade: %+v", got)
	}
}

// TestPersist_RecoveryClearsDegraded — the recovery transition: cold-start load
// fails (Degraded true), then a subsequent SUCCESSFUL Load flips Degraded false +
// loaded true and the policy is present.
func TestPersist_RecoveryClearsDegraded(t *testing.T) {
	pool := guardrailTestPool(t)
	ctx := context.Background()
	// seed a good policy in the DB.
	seed := newStoreEngine(pool)
	if err := seed.SetPolicy(ctx, "ws-rec", fullPolicy("ws-rec")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	e := New(pii.New(), injection.New(injection.DefaultPolicy()))
	e.store = errStore{failQuery: true} // cold start with the DB unreachable
	if err := e.Load(ctx); err == nil || !e.Degraded() {
		t.Fatalf("cold-start failure must error + set Degraded(); err nil? degraded=%v", e.Degraded())
	}
	// DB recovers.
	e.SetStore(pool)
	if err := e.Load(ctx); err != nil {
		t.Fatalf("recovery Load: %v", err)
	}
	if e.Degraded() {
		t.Error("a successful load must CLEAR Degraded()")
	}
	if got := e.GetPolicy("ws-rec"); got.PIIAction != ActionBlock {
		t.Error("after recovery, the custom policy must be present")
	}
}

// TestPersist_ConcurrentReaderVsReload_NoRace — readers racing a reload under
// -race see a consistent map.
func TestPersist_ConcurrentReaderVsReload_NoRace(t *testing.T) {
	pool := guardrailTestPool(t)
	ctx := context.Background()
	e := newStoreEngine(pool)
	if err := e.SetPolicy(ctx, "ws-race", fullPolicy("ws-race")); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if err := e.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = e.GetPolicy("ws-race")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = e.Reload(ctx)
		}
	}()
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestPersist_JSONBAcrossExecModes — the #133 guard for guardrail_policies. The
// policy JSONB write MUST go through dbjson.Marshal (text-encoded), so it lands as
// JSON under BOTH the SimpleProtocol that LENS_DB_PGBOUNCER forces (transaction
// pooling can't keep prepared statements → pgx would infer a raw []byte param as
// bytea and Postgres rejects the hex with 22P02) AND the extended/direct default.
// On an unfixed tree (raw []byte) the simple_protocol subtest FAILS with 22P02.
// Mirrors internal/mining/jsonb_simple_protocol_test.go.
func TestPersist_JSONBAcrossExecModes(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG jsonb protocol guard")
	}
	for _, tc := range []struct {
		name string
		mode pgx.QueryExecMode
	}{
		{"simple_protocol_pgbouncer", pgx.QueryExecModeSimpleProtocol},
		{"extended_protocol_direct", pgx.QueryExecModeCacheStatement},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cfg, err := pgxpool.ParseConfig(url)
			if err != nil {
				t.Fatal(err)
			}
			cfg.ConnConfig.DefaultQueryExecMode = tc.mode
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			for _, ddl := range []string{
				`DROP TABLE IF EXISTS guardrail_policies`,
				`CREATE TABLE guardrail_policies (
					workspace_id TEXT PRIMARY KEY,
					policy       JSONB NOT NULL,
					created_at   TIMESTAMPTZ DEFAULT NOW(),
					updated_at   TIMESTAMPTZ DEFAULT NOW())`,
			} {
				if _, err := pool.Exec(ctx, ddl); err != nil {
					t.Fatalf("schema: %v", err)
				}
			}
			e := newStoreEngine(pool)
			if err := e.SetPolicy(ctx, "ws-proto", fullPolicy("ws-proto")); err != nil {
				t.Fatalf("SetPolicy under %s exec mode: %v", tc.name, err)
			}
			// A json operator works only if the value landed as JSON, not bytea hex.
			var piiAction string
			if err := pool.QueryRow(ctx,
				`SELECT policy->>'pii_action' FROM guardrail_policies WHERE workspace_id='ws-proto'`,
			).Scan(&piiAction); err != nil {
				t.Fatalf("read back policy jsonb under %s: %v", tc.name, err)
			}
			if piiAction != string(ActionBlock) {
				t.Fatalf("policy jsonb not persisted as JSON under %s: want block, got %q", tc.name, piiAction)
			}
			// And the engine reloads it field-identically across the protocol.
			if err := e.Reload(ctx); err != nil {
				t.Fatalf("Reload under %s: %v", tc.name, err)
			}
			if got := e.GetPolicy("ws-proto"); got.PIIAction != ActionBlock || len(got.BlockedTopics) != 2 || len(got.CustomRules) != 1 {
				t.Fatalf("reloaded policy wrong under %s: %+v", tc.name, got)
			}
		})
	}
}
