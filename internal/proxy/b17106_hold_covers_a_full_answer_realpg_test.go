package proxy

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
)

// B17.106 — the 9 Oct testers' agent request, reproduced. Claude Sonnet 5, max_tokens 16, a 48-character sum: the
// provider counted 25 input tokens, and the answer that ran on past the number used all 16 output tokens, 2100 µLXC
// at the catalog price. The hold counted len(prompt)/4 = 12 input tokens, 1840 µLXC, so the settle cut the charge to
// 1840 and wrote 260 off, and the ledger came up short of the answers the testers were shown.

const b17106Question = "What is 4219 + 5977? Reply with the number only."

func TestB17106_AnAgentsAnswerThatUsesAllItsMaxTokensIsChargedItsUsage(t *testing.T) {
	const answer = "10196\n\n**4219 + 5977 = 10196"
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streamed"}[stream], func(t *testing.T) {
			p, pool, _ := anthropicPoolProxy(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), `"stream":true`) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, chatSSEWithUsage(answer, 25, 16))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_b17106","type":"message","role":"assistant","model":"claude-sonnet-5",`+
					`"content":[{"type":"text","text":`+jsonString(answer)+`}],"stop_reason":"max_tokens",`+
					`"usage":{"input_tokens":25,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":16}}`)
			}))
			t.Cleanup(up.Close)
			p.anthropicURL = up.URL

			body := `{"model":"claude-sonnet-5","max_tokens":16,"stream":` + strconv.FormatBool(stream) +
				`,"messages":[{"role":"user","content":"` + b17106Question + `"}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Talyvor-Workspace", "wsPoolA")
			req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "agent-wsPoolA", WorkspaceID: "wsPoolA"}))
			w := httptest.NewRecorder()
			p.HandleAnthropic(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}

			var held int64
			if err := pool.QueryRow(context.Background(), `SELECT -amount FROM lxc_ledger
				WHERE workspace_id = 'wsPoolA' AND type = 'reservation_hold'`).Scan(&held); err != nil {
				t.Fatal(err)
			}
			usd, _ := alerts.CostUSDResolved("claude-sonnet-5", catalog.PurposeCharge, 25, 0, 0, 16)
			want := int64(math.Ceil(usd / economy.LXCUSDValue * 1e6))
			charged, meta := spendRow(t, pool, "wsPoolA")
			if -charged != want {
				t.Fatalf("the answer was charged %d µLXC against a hold of %d, want its usage (25 in, 16 out) at the "+
					"catalog price: %d — a hold below the answer's cost cuts the charge to the hold", -charged, held, want)
			}
			if _, cut := meta["written_off_ulxc"]; cut {
				t.Errorf("the spend row was cut to its hold of %d µLXC: %v", held, meta)
			}
		})
	}
}
