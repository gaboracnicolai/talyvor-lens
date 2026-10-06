package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/catalog"
)

// B35.3 — the cause the 6 Oct testers found, reproduced. Every agent request charged exactly its hold (971 µLXC
// on Free, 920 + 51) asked a question a tester had asked on an earlier night, so it was served from the shared
// synthetic pool. That serve was priced on len(served)/4 output tokens — the whole stored JSON reply — about
// 124 tokens for "9964" against the 16 its max_tokens allowed, so the settle cut it to the hold while the
// X-Talyvor-Pool-Charged-ULXC header said about 4,430. Priced on the stored reply's own usage, it is inside the
// hold and the header is the ledger row.

const b353Question = "What is 3997 + 5967? Reply with the number only."

// b353Reply is Anthropic's reply as the testers received it: a four-character answer, its usage, and the
// envelope around them.
const b353Reply = `{"id":"msg_01XFDUDYJgAACzvnptvVoYEL","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001",` +
	`"content":[{"type":"text","text":"9964"}],"stop_reason":"end_turn","stop_sequence":null,` +
	`"usage":{"input_tokens":22,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,` +
	`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens":4,"service_tier":"standard"}}`

func TestB353_AnAgentsPooledAnswerIsChargedItsUsageInsideItsHold(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streamed"}[stream], func(t *testing.T) {
			p, pool, _ := anthropicPoolProxy(t)
			p.SetPoolConsumerDiscount(0.30)
			var calls int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt64(&calls, 1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, b353Reply)
			}))
			t.Cleanup(up.Close)
			p.anthropicURL = up.URL

			ask := func(ws string, stream bool) *httptest.ResponseRecorder {
				body := `{"model":"claude-haiku-4-5","max_tokens":16,"stream":` + strconv.FormatBool(stream) +
					`,"messages":[{"role":"user","content":"` + b353Question + `"}]}`
				req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Talyvor-Workspace", ws)
				req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "agent-" + ws, WorkspaceID: ws}))
				w := httptest.NewRecorder()
				p.HandleAnthropic(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("%s: status %d: %s", ws, w.Code, w.Body.String())
				}
				return w
			}
			ask("wsPoolA", false) // an earlier night's tester: the model answers and the answer is pooled
			served := ask("wsPoolB", stream)
			if n := atomic.LoadInt64(&calls); n != 1 {
				t.Fatalf("the provider was called %d times, want 1 — the second agent was not served from the pool", n)
			}

			var held int64
			if err := pool.QueryRow(context.Background(), `SELECT -amount FROM lxc_ledger
				WHERE workspace_id = 'wsPoolB' AND type = 'reservation_hold'`).Scan(&held); err != nil {
				t.Fatal(err)
			}
			usage, _ := alerts.CostUSDResolved("claude-haiku-4-5", catalog.PurposeCharge, 22, 0, 0, 4)
			want := pricePooledHit(usage, 0.30, "")
			charged, meta := spendRow(t, pool, "wsPoolB")
			if -charged != want.ChargedULXC || want.ChargedULXC >= held {
				t.Fatalf("the pooled answer was charged %d µLXC against a hold of %d, want its usage (22 in, 4 out) at "+
					"the catalog price less the pool discount: %d, inside the hold", -charged, held, want.ChargedULXC)
			}
			if h := served.Header().Get("X-Talyvor-Pool-Charged-ULXC"); h != strconv.FormatInt(-charged, 10) {
				t.Errorf("X-Talyvor-Pool-Charged-ULXC is %s and the ledger charged %d — the header must be the row", h, -charged)
			}
			if meta["pool_list_ulxc"] != float64(want.ListULXC) {
				t.Errorf("pool_list_ulxc = %v, want %d: the usage at the catalog price", meta["pool_list_ulxc"], want.ListULXC)
			}
			if _, cut := meta["written_off_ulxc"]; cut {
				t.Errorf("the pooled answer's spend row was cut to its hold: %v", meta)
			}
		})
	}
}
