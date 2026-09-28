package proxy

import (
	"context"
	"io"
	"math"
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
)

// B21.1 — A REPHRASED QUESTION THAT NAMES SOMETHING OR HAS A NUMBER IS ANSWERED FROM THE CACHE.
//
// Nicolai's "which city is the France's capital?" after "what is the capital of France?" went to the
// model: a question with an entity took the entity lane, whose floor was the 0.98 threshold, so the
// rephrasing never became a candidate and the verifier was never asked. Here every distinct text gets
// its own vector at cosine 0.7225 to every other — between EntityLowerBound and the threshold — and
// the verifier says YES only to the two real rephrasings. Private read first, then the pool; one side
// streamed and the other buffered. On main the rephrasings go to the model.

// b211Embedder gives each distinct text 0.85·e0 + 0.5268·e_k with its own k: any two texts are 0.7225 apart.
type b211Embedder struct {
	mu   sync.Mutex
	axis map[string]int
}

func (*b211Embedder) Model() string { return "text-embedding-3-small" }

func (e *b211Embedder) Embed(_ context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	k, ok := e.axis[text]
	if !ok {
		k = len(e.axis) + 1
		e.axis[text] = k
	}
	v := make([]float32, 1536)
	v[0], v[k] = 0.85, float32(math.Sqrt(1-0.85*0.85))
	return v, nil
}

const (
	b211France       = "what is the capital of France?"
	b211FranceAgain  = "which city is the France's capital?"
	b211UK           = "what is the capital of the UK?"
	b211UKAgain      = "which city is the UK's capital?"
	b211ThreePlus3   = "what is 3+3?"
	b211ThreeTimes3  = "what is 3*3?"
	b211ThreeMinus3  = "what is 3-3?"
	b211ThreePlus4   = "what is 3+4?"
	b211Similarity   = 0.85 * 0.85
	b211PooledCharge = `SELECT count(*) FROM lxc_ledger WHERE workspace_id = 'wsPoolB' AND amount < 0 AND metadata ? 'pool_saved_ulxc'`
)

func TestB211_ARephrasingThatNamesSomethingIsServedOnTheVerifiersYes_PrivateAndPooled(t *testing.T) {
	if !(b211Similarity >= cache.EntityLowerBound && b211Similarity < 0.98) {
		t.Fatalf("the fixture's similarity %.4f must sit between EntityLowerBound %.2f and the 0.98 threshold", b211Similarity, cache.EntityLowerBound)
	}
	for _, pair := range [][2]string{{b211France, b211FranceAgain}, {b211UK, b211UKAgain}, {b211ThreePlus3, b211ThreeTimes3}, {b211ThreePlus3, b211ThreeMinus3}} {
		if a, b := discriminator.Canon(pair[0]), discriminator.Canon(pair[1]); !a.Verifiable() || a != b {
			t.Fatalf("%q (%q) and %q (%q) must name the same entities — possessives included", pair[0], a, pair[1], b)
		}
	}

	p, ledger, _ := chatCacheProxy(t, "")
	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, chatSSE("An answer.", true))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant",`+
			`"content":[{"type":"text","text":"An answer."}],`+
			`"model":"claude-haiku-4-5","usage":{"input_tokens":16,"output_tokens":9}}`)
	}))
	t.Cleanup(up.Close)
	p.anthropicURL = up.URL

	vectors := fpDB(t)
	emb := &b211Embedder{axis: map[string]int{}}
	sem := cache.NewSemanticCache(vectors, emb, 0.98, time.Hour)
	v := &b97Verifier{same: map[[2]string]bool{{b211France, b211FranceAgain}: true, {b211UK, b211UKAgain}: true}}
	sem.SetPairVerifier(v)
	p.semantic, p.embedder = sem, emb

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
	modelCalls := func(want int64, why string) {
		t.Helper()
		if got := atomic.LoadInt64(&calls); got != want {
			t.Fatalf("the model was asked %d times, want %d — %s", got, want, why)
		}
	}
	pooledCharges := func() int {
		t.Helper()
		var n int
		if err := ledger.QueryRow(context.Background(), b211PooledCharge).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// PRIVATE: the same workspace rephrases its own question, in its own conversation.
	ask("wsPoolA", b211France, true)
	ask("wsPoolA", b211FranceAgain, false)
	modelCalls(1, "\"which city is the France's capital?\" was not answered from the workspace's own earlier answer")

	// POOLED: another workspace asks A's question in other words, and is charged a pooled answer.
	ask("wsPoolA", b211UK, false)
	ask("wsPoolB", b211UKAgain, true)
	modelCalls(2, "\"which city is the UK's capital?\" was not answered from the pool")
	if got := pooledCharges(); got != 1 {
		t.Fatalf("wsPoolB has %d pooled-answer charge rows, want 1", got)
	}

	// DANGER, same entities: 3*3 and 3-3 are not 3+3. The verifier says NO, privately and in the pool.
	if _, err := vectors.Exec(context.Background(), `TRUNCATE prompt_embeddings`); err != nil {
		t.Fatal(err)
	}
	ask("wsPoolA", b211ThreePlus3, false)
	ask("wsPoolA", b211ThreeTimes3, true)
	modelCalls(4, "\"what is 3*3?\" was served the answer to \"what is 3+3?\"")
	ask("wsPoolB", b211ThreeMinus3, false)
	modelCalls(5, "wsPoolB's \"what is 3-3?\" was served another sum's answer from the pool on a NO")
	// A different number is never even a candidate: the discriminators must be exactly equal.
	ask("wsPoolB", b211ThreePlus4, true)
	modelCalls(6, "\"what is 3+4?\" was served another sum's answer")
	if got := pooledCharges(); got != 1 {
		t.Errorf("wsPoolB has %d pooled-answer charge rows after the refused pairs, want still 1", got)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	for _, seen := range v.seen {
		if seen[1] == b211ThreePlus4 {
			t.Errorf("the verifier was asked about %q — its numbers match no stored question, so it is no candidate", seen)
		}
	}
	for _, want := range [][2]string{{b211France, b211FranceAgain}, {b211UK, b211UKAgain}} {
		found := false
		for _, seen := range v.seen {
			found = found || seen == want
		}
		if !found {
			t.Errorf("the verifier was never asked about %q — the rephrasing never became a candidate (asked: %q)", want, v.seen)
		}
	}
}
