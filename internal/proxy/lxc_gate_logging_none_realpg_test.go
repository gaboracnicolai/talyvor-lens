package proxy

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/workspace"
)

// B26.2: with LXC gating on, request logging "none" is gated like every other policy. Since B17.17 the
// debit fires for LoggingNone on both seams, so the old exemption let such a workspace spend past zero.
func TestLXCGate_LoggingNoneIsRefusedAtZeroAndDebitedOnceWhenFunded_BothSeams(t *testing.T) {
	cost := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
	for _, stream := range []bool{false, true} {
		for _, c := range []struct {
			name   string
			funded int64
			code   int
			calls  int64
			debit  int64
		}{
			{"zero balance", 0, http.StatusPaymentRequired, 0, 0},
			{"funded", costWireFunded, http.StatusOK, 1, cost},
		} {
			t.Run(map[bool]string{false: "buffered", true: "streamed"}[stream]+"/"+c.name, func(t *testing.T) {
				_, store, pool := seamProxy(t)
				p, _, _ := newLoggingProxy(t, workspace.LoggingNone)
				p.router = nil
				p.SetLXCSpendSink(store, func() bool { return true })
				p.SetLXCGate(store, func() bool { return true })
				seamFund(t, pool, "ws-log", c.funded)
				var calls int64
				chatUpstream(t, p, stream, &calls)

				if code := driveWithAuth(t, p, &auth.AuthContext{WorkspaceID: "ws-log"}, stream, "b262"); code != c.code {
					t.Fatalf("status = %d, want %d", code, c.code)
				}
				if got := atomic.LoadInt64(&calls); got != c.calls {
					t.Fatalf("upstream calls = %d, want %d", got, c.calls)
				}
				rows, debited, desc := prepaidDebits(t, pool)
				if c.debit == 0 {
					if rows != 0 {
						t.Errorf("ledger = %d debit row(s), %d µLXC, %q; want none", rows, debited, desc)
					}
					return
				}
				if rows != 1 || debited != c.debit || desc != "shadow: AI call billing" {
					t.Errorf("ledger = %d row(s), %d µLXC, %q; want 1, %d µLXC, %q", rows, debited, desc, c.debit, "shadow: AI call billing")
				}
				if bal := seamBalance(t, store, "ws-log"); bal != c.funded-c.debit {
					t.Errorf("balance = %d, want %d", bal, c.funded-c.debit)
				}
			})
		}
	}
}
