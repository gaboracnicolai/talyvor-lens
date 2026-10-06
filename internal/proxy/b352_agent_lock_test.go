package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/metrics"
)

// B35.2 — a hold Lens could not take for its locks says so, as a 503 the agent can send again; it never says
// the balance is short. A real shortfall is still the 402 it always was.
func TestB352_AHoldLensCouldNotTakeIsNotABalanceRefusal(t *testing.T) {
	w := httptest.NewRecorder()
	reason := writeAgentRefusal(w, fmt.Errorf("%w after 6 attempts: %w", economy.ErrLockContention, errors.New("deadlock detected")))
	body := w.Body.String()
	if w.Code != http.StatusServiceUnavailable || reason != "agent_hold_unavailable" ||
		!strings.Contains(body, "could not take the hold") || strings.Contains(body, "insufficient") || strings.Contains(body, "sub-budget") {
		t.Fatalf("lock contention answered %d %q (reason %q), want 503 saying Lens could not take the hold", w.Code, body, reason)
	}

	w = httptest.NewRecorder()
	if reason := writeAgentRefusal(w, economy.ErrInsufficientLXC); w.Code != http.StatusPaymentRequired || reason != "agent_blocked" ||
		!strings.Contains(w.Body.String(), "insufficient balance") {
		t.Fatalf("a real shortfall answered %d %q (reason %q), want the 402 it always was", w.Code, w.Body.String(), reason)
	}
}

// failingSettleSpender holds every request and fails every settle, as one Postgres cancelled on every attempt.
type failingSettleSpender struct{ fundingSpender }

func (failingSettleSpender) SettleLXCReservation(context.Context, string, int64, economy.AgentDebitMeta) (int64, int64, error) {
	return 0, 0, economy.ErrLockContention
}

// A served answer whose settle still fails is never silent: an ERROR log naming the reservation, and the
// lens_agent_settle_failures_total metric.
func TestB352_ASettleThatStillFailsIsAnErrorAndAMetric(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p := &Proxy{agentSpender: failingSettleSpender{}}
	ctx := withReservation(context.Background(), reservationHandle{reservationID: "res-b352", requestID: "req-b352"})
	counted := metrics.AgentSettleFailuresTotal.WithLabelValues("reservation")
	before := testutil.ToFloat64(counted)

	if charged := p.settleReservation(ctx, 0.000054, "claude-haiku-4-5"); charged != 0 {
		t.Fatalf("a failed settle reported %v USD charged, want 0", charged)
	}
	if got := testutil.ToFloat64(counted) - before; got != 1 {
		t.Fatalf("lens_agent_settle_failures_total{path=reservation} rose by %v, want 1", got)
	}
	if line := logs.String(); !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "reservation=res-b352") ||
		!strings.Contains(line, "unbilled") {
		t.Fatalf("the failed settle logged %q, want an ERROR naming reservation res-b352 and that the answer is unbilled", line)
	}
}
