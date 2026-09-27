package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/talyvor/lens/internal/auth"
)

// B18.2 — X-Talyvor-Batch: true IS SERVED AND BILLED EXACTLY LIKE A REQUEST WITHOUT IT.
//
// It used to send a non-streaming claude-* request to Anthropic's batch API ahead of all billing:
// 202, nothing billed, no route to fetch the result. Two identical requests from one funded
// workspace through the real handler and the real LXC money path, one with the header: both are
// answered by the model, and each leaves the same debit on lxc_ledger. The cache is bypassed on
// both so the header is the only difference.
func TestB182_BatchHeaderIsServedAndBilledLikeAnyRequest(t *testing.T) {
	p, pool, calls := anthropicPoolProxy(t)
	const ws = "wsPoolA"

	debits := func() (n int, sum int64) {
		t.Helper()
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*), COALESCE(sum(amount), 0) FROM lxc_ledger WHERE workspace_id = $1 AND amount < 0`, ws).
			Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		return n, sum
	}
	ask := func(batch bool) (status int, body string, rows int, charged int64) {
		t.Helper()
		n0, s0 := debits()
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(
			`{"model":"claude-haiku-4-5","max_tokens":256,"messages":[{"role":"user","content":"what is the capital of France?"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		req.Header.Set(CacheBypassHeader, "bypass")
		if batch {
			req.Header.Set("X-Talyvor-Batch", "true")
		}
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "agent-" + ws, WorkspaceID: ws}))
		w := httptest.NewRecorder()
		p.HandleAnthropic(w, req)
		n1, s1 := debits()
		return w.Code, w.Body.String(), n1 - n0, s0 - s1
	}

	plainStatus, _, plainRows, plainCharged := ask(false)
	batchStatus, batchBody, batchRows, batchCharged := ask(true)

	if plainStatus != http.StatusOK || batchStatus != http.StatusOK {
		t.Fatalf("status without the header %d, with it %d — want both 200 (body: %s)", plainStatus, batchStatus, batchBody)
	}
	if !strings.Contains(batchBody, "Paris.") {
		t.Errorf("the batch-header request was not answered by the model: %s", batchBody)
	}
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Errorf("the model was asked %d times, want 2", got)
	}
	if plainRows == 0 || plainCharged <= 0 {
		t.Fatalf("the plain request left %d debit rows for %d µLXC — the fixture bills nothing, so it proves nothing", plainRows, plainCharged)
	}
	if batchRows != plainRows || batchCharged != plainCharged {
		t.Errorf("with the header: %d debit rows, %d µLXC; without: %d rows, %d µLXC — want the same bill",
			batchRows, batchCharged, plainRows, plainCharged)
	}
	t.Logf("each request debited %d µLXC in %d lxc_ledger row(s), with and without X-Talyvor-Batch", batchCharged, batchRows)
}
