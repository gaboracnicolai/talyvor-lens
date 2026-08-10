package proxy

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/discriminator"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
)

// ⚠ THE READ PATH STATES THE RULE AND THE WRITE PATH BREAKS IT.
//
// GetPooled refuses ahead of Embed, and says why in its own comment: "a prompt that can never be
// served must not cost a paid embedding call to discover that." storeCaches, twelve lines away,
// makes a SECOND billed Embed call on the raw prompt and stores a pooled row for exactly the
// prompts that can never be served — after the empty-extraction fix such a row is unreachable from
// BOTH directions, because GetPooled refuses when the reader's canon is empty and
// `discriminators = $6` never matches the NULL the writer now stores.
//
// Measured offline over the poolsafety corpora (Canon is pure Go, so this costs nothing to
// measure): 118/150 deduped CONSUMER prompts and 19/146 ENGINEERING prompts have an empty
// canonical form. On the consumer lane that is four in five pooled writes paying for an embedding
// that can never be served.
//
// ⚠ THE EXACT POOLED WRITE BESIDE IT IS NOT WASTE AND IS DELIBERATELY NOT GATED: it is keyed on
// byte-identical prompt text, so it needs no entity gate to be servable.

// recordingDB is a SemanticDB that records the statements executed against it. The pooled write is
// asserted on the SQL that reaches the database, not on a Go-side flag, so a fix that merely stops
// setting a variable cannot satisfy this test.
type recordingDB struct {
	mu    sync.Mutex
	execs []string
}

func (d *recordingDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.execs = append(d.execs, sql)
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (d *recordingDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return noRow{}
}

func (d *recordingDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}

// pooledUpserts and privateUpserts classify by the COLUMNS in the statement. ⚠ They cannot both
// key on "workspace_id": the pooled upsert names `contributor_workspace_id`, which CONTAINS that
// substring, so a naive private counter would count every pooled write as private too and the
// private floor would pass no matter what the code did.
func (d *recordingDB) pooledUpserts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, s := range d.execs {
		if strings.Contains(s, "is_poolable") {
			n++
		}
	}
	return n
}

func (d *recordingDB) privateUpserts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, s := range d.execs {
		if strings.Contains(s, "workspace_id") && !strings.Contains(s, "is_poolable") {
			n++
		}
	}
	return n
}

type noRow struct{}

func (noRow) Scan(_ ...any) error { return pgx.ErrNoRows }

// countingEmbedder records the exact text of every embedding call, so the test can tell the
// private embed (on the wsID-prefixed cache prompt) from the pooled one (on the raw prompt).
// The COUNT is the money: each call is an unconditional billed HTTP POST in production.
type countingEmbedder struct {
	mu    sync.Mutex
	texts []string
}

func (e *countingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.texts = append(e.texts, text)
	return []float32{0.1, 0.2, 0.3}, nil
}

func (e *countingEmbedder) Model() string { return "text-embedding-3-small" }

func (e *countingEmbedder) callsFor(raw string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, t := range e.texts {
		if t == raw {
			n++
		}
	}
	return n
}

// newPooledWriteProxy is newPoolingProxy plus a semantic cache and a counting embedder, which the
// existing pooling harness passes as nil — which is exactly why no test has ever observed what the
// pooled WRITE spends.
func newPooledWriteProxy(t *testing.T, global *bool) (*Proxy, *countingEmbedder, *recordingDB) {
	t.Helper()
	db := &recordingDB{}
	emb := &countingEmbedder{}
	sem := cache.NewSemanticCacheWithDB(db, emb, 0.98, time.Hour)

	exact, _ := newExactCacheForTest(t)
	wsm := workspace.New(nil)
	for _, id := range []string{"wsA", "wsB"} {
		if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
			ID: id, Name: id, Active: true, LoggingPolicy: workspace.LoggingMetadata,
		}); err != nil {
			t.Fatal(err)
		}
	}
	p := New(
		exact, sem, emb,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, wsm, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	p.SetPoolGate(cache_pooling.New(func() bool { return *global }, wsm.GetCachePoolable))
	return p, emb, db
}

// optIn turns the global switch on and opts wsA into pooling, so the pooled write branch is live.
func optIn(t *testing.T, p *Proxy) {
	t.Helper()
	if err := p.workspaceManager.SetCachePoolable(context.Background(), "wsA", true); err != nil {
		t.Fatal(err)
	}
}

// ⚠ THE DEFECT. An unverifiable prompt must not buy an embedding for a row that cannot be served.
func TestStoreCaches_UnverifiablePromptBuysNoPooledEmbedding(t *testing.T) {
	global := true
	p, emb, db := newPooledWriteProxy(t, &global)
	optIn(t, p)

	const raw = "How long should I boil an egg?"
	if discriminator.Canon(raw).Verifiable() {
		t.Fatalf("fixture is not the case under test: %q has a verifiable canon", raw)
	}

	p.storeCaches(context.Background(), "openai", "gpt-4o", "wsA:"+raw, raw, "wsA", []byte(okResp))

	if n := emb.callsFor(raw); n != 0 {
		t.Errorf("pooled embedding calls on an unverifiable prompt = %d, want 0 "+
			"(each is a billed HTTP POST for a row `discriminators = $6` can never match)", n)
	}
	if n := db.pooledUpserts(); n != 0 {
		t.Errorf("pooled upserts on an unverifiable prompt = %d, want 0", n)
	}
}

// ⚠ THE FLOOR — without it, "refuse the unservable" is satisfied by refusing EVERYTHING, which
// would silently turn pooling off. This is the C5 shape: it must stay green.
func TestStoreCaches_VerifiablePromptStillPoolsAndStillPaysExactlyOnce(t *testing.T) {
	global := true
	p, emb, db := newPooledWriteProxy(t, &global)
	optIn(t, p)

	const raw = "How do I write a Pydantic v2 field validator?"
	if !discriminator.Canon(raw).Verifiable() {
		t.Fatalf("fixture is not the case under test: %q has an empty canon", raw)
	}

	p.storeCaches(context.Background(), "openai", "gpt-4o", "wsA:"+raw, raw, "wsA", []byte(okResp))

	if n := emb.callsFor(raw); n != 1 {
		t.Errorf("pooled embedding calls on a verifiable prompt = %d, want exactly 1 "+
			"(0 means the refusal is too wide and pooling is off; >1 means it is paying twice)", n)
	}
	if n := db.pooledUpserts(); n != 1 {
		t.Errorf("pooled upserts on a verifiable prompt = %d, want 1", n)
	}
}

// ⚠ THE PRIVATE HALF IS UNTOUCHED. The refusal is scoped to the POOLED write; a workspace's own
// semantic cache still stores every prompt, verifiable or not. Narrowing the wrong one would be a
// silent cache regression on the private path, which no pooling test would catch.
func TestStoreCaches_PrivateSemanticWriteSurvivesAnUnverifiablePrompt(t *testing.T) {
	global := true
	p, emb, db := newPooledWriteProxy(t, &global)
	optIn(t, p)

	const raw = "How long should I boil an egg?"
	cachePrompt := "wsA:" + raw

	p.storeCaches(context.Background(), "openai", "gpt-4o", cachePrompt, raw, "wsA", []byte(okResp))

	if n := emb.callsFor(cachePrompt); n != 1 {
		t.Errorf("private embedding calls = %d, want 1 — the private cache must still store "+
			"prompts that name nothing", n)
	}
	// ⚠ THE EMBED CALL IS NOT THE STORE. Embed sits in the `if` init statement, so a refusal
	// bolted onto that condition still PAYS for the embedding and merely drops the row — the
	// call count alone cannot see it. Assert the statement that reached the database.
	if n := db.privateUpserts(); n != 1 {
		t.Errorf("private semantic upserts = %d, want 1", n)
	}
}

// ⚠ THE EXACT POOLED WRITE MUST STAY UNGATED, AND UNTIL NOW NOTHING CHECKED THAT.
// TestPooling_AllOn_CrossTenantHit is the cross-tenant floor, but its fixture is "what is 2+2",
// whose canonical form is `num:2` — VERIFIABLE. So gating the exact write on the entity check
// leaves that test green: the corpus could not tell the mutation apart. This is the same scenario
// with a prompt that actually has an empty canon, which is the only shape that can catch it.
func TestPooling_AllOn_CrossTenantHit_UnverifiablePrompt(t *testing.T) {
	global := true
	p, wsm, _, _, calls := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)

	const raw = "How long should I boil an egg?"
	if discriminator.Canon(raw).Verifiable() {
		t.Fatalf("fixture is not the case under test: %q has a verifiable canon", raw)
	}

	dispatchWS(t, p, "wsA", raw)
	before := atomic.LoadInt64(calls)
	dispatchWS(t, p, "wsB", raw)
	if atomic.LoadInt64(calls)-before != 0 {
		t.Errorf("a prompt that names nothing is still EXACTLY the same prompt: wsB must be "+
			"served from the pooled exact cache; delta=%d want 0", atomic.LoadInt64(calls)-before)
	}
}
