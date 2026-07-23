package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/session"
	"github.com/talyvor/lens/internal/workspace"
)

// fakeLearner captures every TokenEvent the proxy hands the routing learner, so
// a test can assert on WHAT THE LEARNER RECEIVED — not on a status code. The
// learner persists the raw prompt+response to a 30-day NATS stream, and the
// request succeeds whether or not the event is published, so the only way to
// prove the logging-policy gate is to watch the learner's inbox.
type fakeLearner struct {
	mu     sync.Mutex
	events []learner.TokenEvent
}

func (f *fakeLearner) Record(_ context.Context, e learner.TokenEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

func (f *fakeLearner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

// fakeOllama stands up a serving *localrouter.LocalRouter backed by a mock
// Ollama (the two endpoints ShouldUseLocal + Forward touch), so tryLocalRouting
// actually serves locally and reaches its learner + RecordSpend recording block.
func fakeOllama(t *testing.T) *localrouter.LocalRouter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = io.WriteString(w, `{"models":[{"name":"llama3.2:latest","size":1.0,"is_available":true}]}`)
		case "/api/generate":
			_, _ = io.WriteString(w, `{"response":"the local model produced these output bytes","done":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	lr := localrouter.New(srv.URL)
	lr.SetHTTPClient(srv.Client())
	if !lr.CheckAvailability(context.Background()) {
		t.Fatal("fakeOllama: availability check failed — mock /api/tags not wired")
	}
	return lr
}

// learnerPolicyCases: the learner persists prompt+response CONTENT, so — like
// the upstream path (proxy.go ~1435) — it must fire ONLY on LoggingFull. None
// (the privacy escape hatch) AND metadata (cost/tokens only, prompt_text
// stripped) both opt out of content persistence, so both must send the learner
// ZERO events. Only full yields one.
var learnerPolicyCases = []struct {
	policy    workspace.LoggingPolicy
	wantEvent int
}{
	{workspace.LoggingNone, 0},
	{workspace.LoggingMetadata, 0},
	{workspace.LoggingFull, 1},
}

// TestLearner_NodePath_HonoursLoggingPolicy drives tryNodeRouting (the node-serve
// path) to a real serve and asserts the learner received an event ONLY under
// full. RED before the fix: the node-serve recordTokenEvent call is ungated, so
// none/metadata leak the prompt+response to the 30-day learner log.
func TestLearner_NodePath_HonoursLoggingPolicy(t *testing.T) {
	for _, tc := range learnerPolicyCases {
		t.Run(string(tc.policy), func(t *testing.T) {
			p, _, _ := newLoggingProxy(t, tc.policy) // registers ws-log with tc.policy
			fl := &fakeLearner{}
			p.learner = fl

			node := fakeInferenceNode(t, "the node produced these output bytes right here", 9, 7)
			wireNode(t, p, "node-model", node.URL, node.Client())

			rec := httptest.NewRecorder()
			served := p.tryNodeRouting(rec, context.Background(),
				"vllm", "node-model", "a prompt of a certain length here", "a prompt of a certain length here",
				"ws-log", "", "", "feat", "sess", "node-req-learner", false, "", localrouter.RoutingStrategy(""))
			if !served {
				t.Fatalf("tryNodeRouting returned false (status=%d body=%s)", rec.Code, rec.Body.String())
			}
			if got := fl.count(); got != tc.wantEvent {
				t.Errorf("node path, policy %q: learner received %d events, want %d (none/metadata must not reach the learner)", tc.policy, got, tc.wantEvent)
			}
		})
	}
}

// TestLearner_LocalPath_HonoursLoggingPolicy drives tryLocalRouting (the local
// Ollama path) to a real serve and asserts BOTH persistence sinks on that path
// honour the policy: the learner (content) fires only under full, and the
// RecordSpend token_events write is skipped for none and prompt-stripped for
// metadata — matching the upstream idiom. RED before the fix: the local path's
// recordTokenEvent AND its RecordSpend are both ungated.
func TestLearner_LocalPath_HonoursLoggingPolicy(t *testing.T) {
	for _, tc := range learnerPolicyCases {
		t.Run(string(tc.policy), func(t *testing.T) {
			p, sink, _ := newLoggingProxy(t, tc.policy)
			// ShouldUseLocal only serves wsID ""/"default"; register "default"
			// with this policy so GetLoggingPolicy sees it on the local path.
			if err := p.workspaceManager.RegisterWorkspace(context.Background(), workspace.Workspace{
				ID: "default", Name: "default-ws", Active: true, LoggingPolicy: tc.policy,
			}); err != nil {
				t.Fatalf("RegisterWorkspace(default): %v", err)
			}
			fl := &fakeLearner{}
			p.learner = fl
			p.localRouter = fakeOllama(t)

			rec := httptest.NewRecorder()
			served := p.tryLocalRouting(rec, context.Background(),
				"openai", "gpt-4", "hi", "hi",
				"default", "", "", "feat", "sess", "local-req-learner", false, "", localrouter.RoutingStrategy(""))
			if !served {
				t.Fatalf("tryLocalRouting returned false (status=%d body=%s)", rec.Code, rec.Body.String())
			}

			// (1) Learner (content) — full only.
			if got := fl.count(); got != tc.wantEvent {
				t.Errorf("local path, policy %q: learner received %d events, want %d", tc.policy, got, tc.wantEvent)
			}

			// (2) RecordSpend (token_events) — skipped for none, prompt stripped for metadata.
			switch tc.policy {
			case workspace.LoggingNone:
				if sink.calls != 0 {
					t.Errorf("local path, none: RecordSpend called %d times, want 0 (none persists nothing)", sink.calls)
				}
			case workspace.LoggingMetadata:
				if sink.calls != 1 {
					t.Errorf("local path, metadata: RecordSpend called %d times, want 1", sink.calls)
				}
				if sink.lastPrompt != "" {
					t.Errorf("local path, metadata: RecordSpend prompt = %q, want empty (metadata strips prompt_text)", sink.lastPrompt)
				}
			case workspace.LoggingFull:
				if sink.calls != 1 {
					t.Errorf("local path, full: RecordSpend called %d times, want 1", sink.calls)
				}
				if sink.lastPrompt != "hi" {
					t.Errorf("local path, full: RecordSpend prompt = %q, want the original prompt", sink.lastPrompt)
				}
			}
		})
	}
}

// TestSessionTurn_CacheHit_HonoursLoggingNone proves the cache-hit session-turn
// write (proxy.go ~973) honours LoggingNone. Its sibling — the upstream turn
// write (~1390) — already skips None, but the cache-hit path did not, so a none
// workspace's raw prompt+response were retained in the in-memory session on
// every cache hit (the turn's Prompt/Response are in-memory only, never a DB
// row — a weaker exposure than the learner's 30-day log, but still against the
// policy its sibling honours). Warm the cache (miss), then hit it: none must
// retain ZERO turns; full retains both (miss + hit).
func TestSessionTurn_CacheHit_HonoursLoggingNone(t *testing.T) {
	cases := []struct {
		policy    workspace.LoggingPolicy
		wantTurns int
	}{
		{workspace.LoggingNone, 0}, // miss skips (1390 None-gate), hit must skip too (973)
		{workspace.LoggingFull, 2}, // miss records + cache hit records
	}
	for _, tc := range cases {
		t.Run(string(tc.policy), func(t *testing.T) {
			p, _, _ := newLoggingProxy(t, tc.policy)
			p.sessionTracker = session.New(nil)
			const sid = "sess-cache-turn"

			for i := 0; i < 2; i++ { // 1st = miss→upstream+cache-store, 2nd = exact cache hit
				body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`
				req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Talyvor-Workspace", "ws-log")
				req.Header.Set("X-Talyvor-Session", sid)
				w := httptest.NewRecorder()
				p.HandleOpenAI(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("dispatch %d: status %d body=%s", i, w.Code, w.Body.String())
				}
			}

			sess, ok := p.sessionTracker.GetSession(sid)
			if !ok {
				t.Fatalf("session %q not found (GetOrCreate should have made it regardless of policy)", sid)
			}
			if sess.TurnCount != tc.wantTurns {
				t.Errorf("policy %q: session TurnCount = %d, want %d (none must not retain prompt/response, even on a cache hit)", tc.policy, sess.TurnCount, tc.wantTurns)
			}
		})
	}
}

// TestLearner_StreamPath_HonoursLoggingPolicy drives the streaming SSE path
// (stream.go's post-stream recording) and asserts the learner is fed ONLY under
// full. The SWEEP found this FOURTH call site — it too was ungated, so a
// none/metadata STREAMED request (the Claude-Code hot path) leaked
// prompt+response to the 30-day learner log.
func TestLearner_StreamPath_HonoursLoggingPolicy(t *testing.T) {
	for _, tc := range learnerPolicyCases {
		t.Run(string(tc.policy), func(t *testing.T) {
			p, _, _ := newLoggingProxy(t, tc.policy)
			p.openAIURL = sseUpstream(t, openAISSEBody).URL
			fl := &fakeLearner{}
			p.learner = fl

			body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"stream":true}`
			req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Talyvor-Workspace", "ws-log")
			w := newFlushRecorder()
			p.HandleOpenAI(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if got := fl.count(); got != tc.wantEvent {
				t.Errorf("stream path, policy %q: learner received %d events, want %d", tc.policy, got, tc.wantEvent)
			}
		})
	}
}

// TestLearner_UpstreamPath_HonoursLoggingPolicy locks the precedent (proxy.go
// ~1435) end-to-end and proves the chokepoint gate does not suppress the full
// case (the positive check that the learner is fed when it should be). Green
// before AND after the fix — a regression guard on the working path.
func TestLearner_UpstreamPath_HonoursLoggingPolicy(t *testing.T) {
	for _, tc := range learnerPolicyCases {
		t.Run(string(tc.policy), func(t *testing.T) {
			p, _, _ := newLoggingProxy(t, tc.policy)
			fl := &fakeLearner{}
			p.learner = fl

			w := dispatch(t, p, "ws-log")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if got := fl.count(); got != tc.wantEvent {
				t.Errorf("upstream path, policy %q: learner received %d events, want %d", tc.policy, got, tc.wantEvent)
			}
		})
	}
}
