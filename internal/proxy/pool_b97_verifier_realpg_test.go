package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/discriminator"
	"github.com/talyvor/lens/internal/pairverify"
)

// B9.7 — A REPHRASED QUESTION WITH NO ENTITY IS ANSWERED FROM THE POOL, ON THE VERIFIER'S YES.
//
// Before B9.7 an entity-free question could never be served from the pool: the entity gate had
// nothing to compare, so GetPooled refused it before it queried, and storeCaches never wrote it.
// This drives the browser chat (a session key) through the real handler, with a real semantic cache
// on pgvector, the pool on and both workspaces opted in and funded. The embedder returns one vector
// for every text, so every stored question is a candidate; the ONLY thing that decides a serve is
// the verifier, stubbed to say YES for the rephrasing and NO for the danger pair.
//
// One side of each pair is streamed and the other buffered, so both seams write and both read.

// b97Verifier says YES only for the pairs it was given, and records every pair it was asked about.
type b97Verifier struct {
	mu   sync.Mutex
	same map[[2]string]bool
	seen [][2]string
}

func (v *b97Verifier) Verify(_ context.Context, stored, asked string) (pairverify.Verdict, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = append(v.seen, [2]string{stored, asked})
	return pairverify.Verdict{Same: v.same[[2]string{stored, asked}]}, nil
}

// Corpus pairs (internal/poolsafety): a consumer rephrasing and a consumer danger pair, neither
// naming an entity.
const (
	b97Stored   = "Can cats eat chocolate?"
	b97Rephrase = "Is chocolate poisonous to cats?"
	b97Tenant   = "How much notice must I give my landlord?"
	b97Landlord = "How much notice must my landlord give me?"
)

func TestPoolB97_EntityFreeRephrasingIsServedFromThePoolOnlyOnTheVerifiersYes(t *testing.T) {
	for _, q := range []string{b97Stored, b97Rephrase, b97Tenant, b97Landlord} {
		if discriminator.Canon(q).Verifiable() {
			t.Fatalf("%q names an entity (%q) — this test is about the lane for questions that name none", q, discriminator.Canon(q))
		}
	}
	p, ledger, _ := chatCacheProxy(t, "")
	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, chatSSE("No. Chocolate is toxic to cats.", true))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant",`+
			`"content":[{"type":"text","text":"No. Chocolate is toxic to cats."}],`+
			`"model":"claude-haiku-4-5","usage":{"input_tokens":16,"output_tokens":9}}`)
	}))
	t.Cleanup(up.Close)
	p.anthropicURL = up.URL

	vectors := fpDB(t)
	sem := cache.NewSemanticCache(vectors, fpEmbedder{}, 0.98, time.Hour)
	v := &b97Verifier{same: map[[2]string]bool{{b97Stored, b97Rephrase}: true}}
	sem.SetPairVerifier(v)
	p.semantic, p.embedder = sem, fpEmbedder{}

	ask := func(ws, question string, stream bool) {
		t.Helper()
		body := `{"model":"claude-haiku-4-5","max_tokens":4096,"messages":[{"role":"user","content":"` + question + `"}]}`
		if stream {
			body = strings.Replace(body, `"max_tokens":4096,`, `"max_tokens":4096,"stream":true,`, 1)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
		w := newFlushRecorder()
		p.HandleAnthropic(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("ws=%s %q status=%d body=%s", ws, question, w.Code, w.Body.String())
		}
	}
	pooledCharges := func() int {
		t.Helper()
		var n int
		if err := ledger.QueryRow(context.Background(),
			`SELECT count(*) FROM lxc_ledger WHERE workspace_id = 'wsPoolB' AND amount < 0 AND metadata ? 'pool_saved_ulxc'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// THE REPHRASING: A asks streamed (the stream seam writes), B asks buffered in other words.
	ask("wsPoolA", b97Stored, true)
	var kept string
	if err := vectors.QueryRow(context.Background(),
		`SELECT prompt_text FROM prompt_embeddings WHERE is_poolable AND contributor_workspace_id = 'wsPoolA'`).Scan(&kept); err != nil || kept != b97Stored {
		t.Fatalf("the pooled row keeps %q (%v), want the contributor's question %q", kept, err, b97Stored)
	}
	ask("wsPoolB", b97Rephrase, false)
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("the model was asked %d times, want 1 — the rephrasing was not served from the pool", got)
	}
	if got := pooledCharges(); got != 1 {
		t.Fatalf("wsPoolB has %d pooled-answer charge rows, want 1", got)
	}

	// THE DANGER PAIR: A asks buffered (the buffered seam writes), B asks the opposite, streamed.
	// The embedder ties every row, so the pool is emptied first: the candidate must be the tenant's.
	if _, err := vectors.Exec(context.Background(), `TRUNCATE prompt_embeddings`); err != nil {
		t.Fatal(err)
	}
	ask("wsPoolA", b97Tenant, false)
	ask("wsPoolB", b97Landlord, true)
	if got := atomic.LoadInt64(&calls); got != 3 {
		t.Fatalf("the model was asked %d times, want 3 — the landlord's question was served the tenant's answer", got)
	}
	if got := pooledCharges(); got != 1 {
		t.Errorf("wsPoolB has %d pooled-answer charge rows after the refused pair, want still 1", got)
	}

	// The verifier judged exactly the two candidate pairs, stored question first.
	want := [][2]string{{b97Stored, b97Rephrase}, {b97Tenant, b97Landlord}}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.seen) != len(want) || v.seen[0] != want[0] || v.seen[1] != want[1] {
		t.Errorf("verifier was asked %q, want %q", v.seen, want)
	}
}
