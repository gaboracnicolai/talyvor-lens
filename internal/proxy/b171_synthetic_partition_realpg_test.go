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
)

// B17.1 — A SYNTHETIC WORKSPACE'S POOLED ANSWERS NEVER CROSS TO A REAL ONE, IN EITHER DIRECTION.
//
// Two poolable, funded workspaces on the real handler, the exact pool and the semantic pool (whose
// embedder ties every text, so only the partition and the verifier decide). wsPoolA is synthetic,
// wsPoolB real: the same words, and a rephrasing the verifier says YES to, go to the model both ways.
// Then wsPoolB turns synthetic too and IS served wsPoolA's answer — a partition, not pooling switched
// off — and the ledger shows the pooled charge and, since B25.2, wsPoolA's royalty for it.

func TestB171_ASyntheticPooledAnswerNeverReachesARealWorkspace_NorARealOneASyntheticWorkspace(t *testing.T) {
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
	sem := cache.NewSemanticCache(vectors, fpEmbedder{}, 0.98, time.Hour)
	sem.SetPairVerifier(&b97Verifier{same: map[[2]string]bool{{b97Stored, b97Rephrase}: true}})
	p.semantic, p.embedder = sem, fpEmbedder{}

	var mu sync.Mutex
	synthetic := map[string]bool{"wsPoolA": true}
	p.SetSyntheticLookup(func(ws string) bool { mu.Lock(); defer mu.Unlock(); return synthetic[ws] })

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
	count := func(sql string) int {
		t.Helper()
		var n int
		if err := ledger.QueryRow(context.Background(), sql).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	const (
		sameWords  = "What is the tallest mountain in the world?"
		otherWords = "How many bones are in the human body?"
	)

	// SYNTHETIC → REAL: the same words (exact pool), then a rephrasing (semantic pool).
	ask("wsPoolA", sameWords, true)
	ask("wsPoolB", sameWords, false)
	modelCalls(2, "a real workspace was served a synthetic workspace's answer from the exact pool")
	ask("wsPoolA", b97Stored, false)
	ask("wsPoolB", b97Rephrase, true)
	modelCalls(4, "a real workspace was served a synthetic workspace's answer from the semantic pool")

	// REAL → SYNTHETIC, the same two ways.
	ask("wsPoolB", otherWords, false)
	ask("wsPoolA", otherWords, true)
	modelCalls(6, "a synthetic workspace was served a real workspace's answer from the exact pool")
	if _, err := vectors.Exec(context.Background(), `TRUNCATE prompt_embeddings`); err != nil {
		t.Fatal(err)
	}
	ask("wsPoolB", b97Stored, true)
	ask("wsPoolA", b97Rephrase, false)
	modelCalls(8, "a synthetic workspace was served a real workspace's answer from the semantic pool")
	if got := count(`SELECT count(*) FROM lxc_ledger WHERE amount < 0 AND metadata ? 'pool_saved_ulxc'`); got != 0 {
		t.Fatalf("%d pooled-answer charges across the partition, want 0", got)
	}

	// SYNTHETIC ↔ SYNTHETIC still pools: wsPoolB turns synthetic and is served wsPoolA's answer.
	mu.Lock()
	synthetic["wsPoolB"] = true
	mu.Unlock()
	ask("wsPoolB", sameWords, true)
	modelCalls(8, "two synthetic workspaces did not share an answer — the partition switched pooling off instead of separating it")
	if got := count(`SELECT count(*) FROM lxc_ledger WHERE workspace_id = 'wsPoolB' AND amount < 0 AND metadata ? 'pool_saved_ulxc'`); got != 1 {
		t.Errorf("wsPoolB has %d pooled-answer charges on its test credits, want 1", got)
	}
	// B25.2: between two test users the contributor earns (marked test on the migrated schema — b252 test).
	if got := count(`SELECT count(*) FROM pool_royalty_mints WHERE requester_workspace_id = 'wsPoolB' AND contributor_workspace_id = 'wsPoolA'`); got != 1 {
		t.Errorf("%d royalty mints for wsPoolA's answer served to wsPoolB, both synthetic; want 1", got)
	}
	if got := count(`SELECT count(*) FROM pool_royalty_mints`); got != 1 {
		t.Errorf("%d royalty mints in all — one crossed the partition; want only the synthetic pair's 1", got)
	}
}
