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
	"github.com/talyvor/lens/internal/tenant"
)

// B18.3 — A SPENDING CAP A CUSTOMER SETS STOPS SPENDING.
//
// The cap is set the way PUT /v1/workspaces/{ws}/config sets it (tenant.Store.UpsertConfig, on a
// fully migrated database) and read back by the real tenant.SpendTracker. It sits between one
// request's estimate and its served cost: the first request is served and billed, and the next —
// the one that would take the month past the cap — is refused before the provider is called. Once
// buffered and once streamed, each on its own funded workspace, asserted on the provider-call counter
// and on lxc_ledger.
func TestB183_ASpendingCapRefusesTheRequestThatWouldExceedIt(t *testing.T) {
	p, ledger, _ := anthropicPoolProxy(t)
	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, chatSSEWithUsage("Paris.", 16, 5000))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"Paris."}],`+
			`"model":"claude-haiku-4-5","usage":{"input_tokens":16,"output_tokens":5000}}`)
	}))
	t.Cleanup(up.Close)
	p.anthropicURL = up.URL

	configs := tenant.NewStore(fpDB(t))
	p.SetWorkspaceLimits(configs, tenant.NewSpendTracker(configs))
	// Above one request's input estimate (~$0.000007), below its served cost: 16 in + 5,000 out on
	// claude-haiku-4-5 is ~$0.025. spending_cap is NUMERIC(12,4), so a cap is stored to 4 decimals.
	const capUSD = 0.01

	debitRows := func(ws string) int {
		t.Helper()
		var n int
		if err := ledger.QueryRow(context.Background(),
			`SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1 AND amount < 0`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ask := func(ws string, stream bool) (int, string) {
		t.Helper()
		body := `{"model":"claude-haiku-4-5","max_tokens":256,"stream":` + map[bool]string{true: "true", false: "false"}[stream] +
			`,"messages":[{"role":"user","content":"what is the capital of France?"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		req.Header.Set(CacheBypassHeader, "bypass")
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "agent-" + ws, WorkspaceID: ws}))
		w := newFlushRecorder()
		p.HandleAnthropic(w, req)
		return w.Code, w.Body.String()
	}

	for _, tc := range []struct {
		ws     string
		stream bool
	}{{"wsPoolA", false}, {"wsPoolB", true}} {
		if err := configs.UpsertConfig(context.Background(), tenant.WorkspaceConfig{ID: tc.ws, SpendingCapUSD: capUSD}); err != nil {
			t.Fatal(err)
		}
		if status, body := ask(tc.ws, tc.stream); status != http.StatusOK {
			t.Fatalf("stream=%v: the first request, within the cap, got %d: %s", tc.stream, status, body)
		}
		calls0, rows0 := atomic.LoadInt64(&calls), debitRows(tc.ws)
		if rows0 == 0 {
			t.Fatalf("stream=%v: the served request left no debit on lxc_ledger — the fixture bills nothing", tc.stream)
		}

		status, body := ask(tc.ws, tc.stream)
		if status != http.StatusPaymentRequired || !strings.Contains(body, "spending cap reached") || !strings.Contains(body, "$0.01") {
			t.Errorf("stream=%v: the request past the cap got %d %s — want 402 naming the cap", tc.stream, status, body)
		}
		if got := atomic.LoadInt64(&calls); got != calls0 {
			t.Errorf("stream=%v: the provider was called for the refused request (%d → %d)", tc.stream, calls0, got)
		}
		if got := debitRows(tc.ws); got != rows0 {
			t.Errorf("stream=%v: the refused request wrote to lxc_ledger (%d → %d debit rows)", tc.stream, rows0, got)
		}
	}
}

// RPM and TPM count what was admitted in the last minute, refuse naming the limit, and admit again
// once the minute has passed.
func TestB183_RateLimitsCountTheLastMinute(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	cfg := &tenant.WorkspaceConfig{ID: "ws", RateLimitRPM: 2, RateLimitTPM: 1000}
	l := newWorkspaceLimits(staticConfigs{cfg}, nil, func() time.Time { return now })
	admit := func(tokens int) (int, string) { return l.admit(context.Background(), "ws", 0, tokens) }

	if s, m := admit(100); s != 0 {
		t.Fatalf("first request refused: %d %s", s, m)
	}
	if s, m := admit(100); s != 0 {
		t.Fatalf("second request refused: %d %s", s, m)
	}
	if s, m := admit(100); s != http.StatusTooManyRequests || !strings.Contains(m, "2 requests a minute") {
		t.Errorf("third request in the minute got %d %q — want 429 naming 2 requests a minute", s, m)
	}

	now = now.Add(61 * time.Second)
	if s, m := admit(900); s != 0 {
		t.Fatalf("a request after the minute passed was refused: %d %s", s, m)
	}
	if s, m := admit(200); s != http.StatusTooManyRequests || !strings.Contains(m, "1000 tokens a minute") {
		t.Errorf("900 + 200 tokens in a minute got %d %q — want 429 naming 1000 tokens a minute", s, m)
	}
}

type staticConfigs struct{ cfg *tenant.WorkspaceConfig }

func (s staticConfigs) GetConfig(context.Context, string) (*tenant.WorkspaceConfig, error) {
	return s.cfg, nil
}
