package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/pairverify"
)

// B16.1 — A CACHED ANSWER NEVER CROSSES INTO ANOTHER CONVERSATION.
//
// Measured 27 Sep, Nicolai's own chat: after "how much is 2+3?" → "2 + 3 = 5" he asked "how much is
// 2+2?" and was served "It's still 5. 🙂" — another chat's answer to "so how much is 2+3?". The
// semantic cache embedded the whole conversation, and two conversations sharing their history embed
// almost identically.
//
// Every conversation here goes through the real handler to real pgvector, and the two things that
// USED to decide a serve are rigged to say yes: the embedder gives every text the same vector, and
// the pair verifier answers YES to every pair. What refuses must therefore be the history hash and
// the entity gate on the latest question. The upstream answers "ANSWER: <latest question>", so a
// served answer names the question it was really for.

type yesVerifier struct{}

func (yesVerifier) Verify(context.Context, string, string) (pairverify.Verdict, error) {
	return pairverify.Verdict{Same: true}, nil
}

type b161Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func b161Body(msgs []b161Msg, stream bool) string {
	b, _ := json.Marshal(map[string]any{"model": "claude-haiku-4-5", "max_tokens": 4096, "stream": stream, "messages": msgs})
	return string(b)
}

func TestB161_AnotherConversationsAnswerIsNeverServed(t *testing.T) {
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
		// The latest question is the last line: the buffered path sends the conversation joined into
		// one message (see ~/talyvor-queue/FOUND.md), the streamed path sends it as it came.
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
	sem.SetPairVerifier(yesVerifier{})
	p.semantic, p.embedder = sem, fpEmbedder{}
	// The exact cache serves byte-identical conversations, rightly, and the private round below
	// asks what the pooled round asks; it is not what this test is about.
	p.exact = nil

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
	empty := func() {
		t.Helper()
		if _, err := vectors.Exec(context.Background(), `TRUNCATE prompt_embeddings`); err != nil {
			t.Fatal(err)
		}
	}
	u := func(s string) b161Msg { return b161Msg{"user", s} }
	a := func(s string) b161Msg { return b161Msg{"assistant", s} }

	traps := []struct {
		name          string
		stored, asked []b161Msg
	}{
		// Nicolai's two conversations: the same history, one digit changed in the last question.
		{"nicolai-2+2-after-2+3",
			[]b161Msg{u("how much is 2+3?"), a("2 + 3 = 5"), u("so how much is 2+3?")},
			[]b161Msg{u("how much is 2+3?"), a("2 + 3 = 5"), u("how much is 2+2?")}},
		// The same last question, whose answer depends on a different history.
		{"follow-up-after-another-history",
			[]b161Msg{u("what is the capital of Germany?"), a("Berlin."), u("and what about France?")},
			[]b161Msg{u("what is the population of Germany?"), a("About 84 million."), u("and what about France?")}},
	}
	for _, path := range []struct{ name, storeWS, askWS string }{
		{"private", "wsPoolA", "wsPoolA"},
		{"pooled", "wsPoolA", "wsPoolB"},
	} {
		for _, tr := range traps {
			empty()
			ask(path.storeWS, tr.stored, true) // the stream seam writes
			before := atomic.LoadInt64(&calls)
			got := ask(path.askWS, tr.asked, false) // the buffered seam reads
			want := "ANSWER: " + tr.asked[len(tr.asked)-1].Content
			if atomic.LoadInt64(&calls) != before+1 || !strings.Contains(got, want) {
				t.Errorf("%s/%s: served from the cache, want the model's %q; got %s", path.name, tr.name, want, got)
			}
		}
	}

	// A rephrased first question in the same workspace IS served — the empty history matches.
	empty()
	ask("wsPoolA", []b161Msg{u("what are the rainbow colours?")}, false)
	before := atomic.LoadInt64(&calls)
	got := ask("wsPoolA", []b161Msg{u("which colours are in a rainbow?")}, true)
	if atomic.LoadInt64(&calls) != before || !strings.Contains(got, "ANSWER: what are the rainbow colours?") {
		t.Errorf("a rephrased single-turn question was not served from the workspace's cache: calls %d→%d, body %s",
			before, atomic.LoadInt64(&calls), got)
	}
}
