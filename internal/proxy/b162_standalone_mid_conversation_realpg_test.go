package proxy

import (
	"context"
	"encoding/json"
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
	"github.com/talyvor/lens/internal/pairverify"
)

// B16.2 — A QUESTION ASKED MID-CONVERSATION REUSES A STANDALONE ANSWER WHEN IT STANDS ALONE.
//
// Measured 27 Sep: "what are the rainbow colours?" asked after an unrelated exchange went to the
// model although the same question had been answered on its own, because the key covered the whole
// conversation. Through the real handler to real pgvector, with every text embedding identically and
// the pair verifier saying YES to identical words: what decides is the stands-alone check, which is
// shown the conversation and says YES only for the question that stands alone.

type b162Verifier struct {
	mu        sync.Mutex
	standsFor map[string]bool // asked question → stands alone in its conversation
	seen      []string        // the histories the stands-alone check was shown
}

func (*b162Verifier) Verify(_ context.Context, stored, asked string) (pairverify.Verdict, error) {
	return pairverify.Verdict{Same: stored == asked}, nil
}

func (v *b162Verifier) StandsAlone(_ context.Context, history, asked string) (pairverify.Verdict, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = append(v.seen, history)
	return pairverify.Verdict{Same: v.standsFor[asked]}, nil
}

func TestB162_AStandaloneAnswerServesTheSameQuestionMidConversation(t *testing.T) {
	p, _, _ := chatCacheProxy(t, "")
	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		var req struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		last := req.Messages[len(req.Messages)-1].Content
		answer := "ANSWER: " + last[strings.LastIndex(last, "\n")+1:]
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, chatSSE(answer, true))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":`+
			jsonString(answer)+`}],"model":"claude-haiku-4-5","usage":{"input_tokens":16,"output_tokens":9}}`)
	}))
	t.Cleanup(up.Close)
	p.anthropicURL = up.URL

	vectors := fpDB(t)
	sem := cache.NewSemanticCache(vectors, fpEmbedder{}, 0.98, time.Hour)
	v := &b162Verifier{standsFor: map[string]bool{"what are the rainbow colours?": true}}
	sem.SetPairVerifier(v)
	p.semantic, p.embedder = sem, fpEmbedder{}
	p.exact = nil // byte-identical conversations are not what this is about

	ask := func(ws string, msgs []b161Msg, stream bool) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(b161Body(msgs, stream)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
		w := newFlushRecorder()
		p.HandleAnthropic(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("ws=%s status=%d body=%s", ws, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	u := func(s string) b161Msg { return b161Msg{"user", s} }
	a := func(s string) b161Msg { return b161Msg{"assistant", s} }
	rainbow := "what are the rainbow colours?"
	unrelated := []b161Msg{u("what is the capital of the UK?"), a("London.")}

	for _, path := range []struct{ name, storeWS, askWS string }{
		{"private", "wsPoolA", "wsPoolA"},
		{"pooled", "wsPoolA", "wsPoolB"},
	} {
		if _, err := vectors.Exec(context.Background(), `TRUNCATE prompt_embeddings`); err != nil {
			t.Fatal(err)
		}
		ask(path.storeWS, []b161Msg{u(rainbow)}, true) // answered on its own, streamed

		before := atomic.LoadInt64(&calls)
		got := ask(path.askWS, append(append([]b161Msg{}, unrelated...), u(rainbow)), false)
		if atomic.LoadInt64(&calls) != before || !strings.Contains(got, "ANSWER: "+rainbow) {
			t.Errorf("%s: the rainbow question after an unrelated exchange was not served the standalone answer: calls %d→%d, body %s",
				path.name, before, atomic.LoadInt64(&calls), got)
		}

		// A follow-up that does not stand alone goes to the model although the words are identical.
		ask(path.storeWS, []b161Msg{u("and what about France?")}, false)
		before = atomic.LoadInt64(&calls)
		ask(path.askWS, []b161Msg{u("what is the capital of Germany?"), a("Berlin."), u("and what about France?")}, true)
		if atomic.LoadInt64(&calls) != before+1 {
			t.Errorf("%s: a follow-up was served a standalone answer", path.name)
		}
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.seen) == 0 || v.seen[0] != "User: what is the capital of the UK?\n\nAssistant: London." {
		t.Errorf("the stands-alone check was shown %q, want the conversation before the question", v.seen)
	}
}
