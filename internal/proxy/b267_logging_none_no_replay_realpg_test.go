package proxy

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/workspace"
)

// B26.7: a workspace that asked a question and then switched request logging to "none" is not replayed
// the answer it kept before the switch. The repeat goes to the model and is charged as a model call —
// docs/retention-none-and-the-semantic-cache.md: "every repeat of its own question goes to the model".
// Both of its own copies are kept before the switch (exact in Redis, semantic in Postgres), so the
// repeat reaches the model only if neither is read.
func TestB267_LoggingNoneIsNeverReplayedAnAnswerKeptBeforeTheSwitch_BothSeams(t *testing.T) {
	cost := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streamed"}[stream], func(t *testing.T) {
			_, store, pool := seamProxy(t)
			p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
			p.router = nil
			db := fpDB(t)
			p.semantic, p.embedder = cache.NewSemanticCache(db, fpEmbedder{}, 0.98, time.Hour), fpEmbedder{}
			p.SetLXCSpendSink(store, func() bool { return true })
			seamFund(t, pool, "ws-log", costWireFunded)
			var calls int64
			chatUpstream(t, p, stream, &calls)
			ask := func(want int64, debits int) {
				t.Helper()
				if code := driveWithAuth(t, p, &auth.AuthContext{WorkspaceID: "ws-log"}, stream, "b267"); code != http.StatusOK {
					t.Fatalf("status = %d, want 200", code)
				}
				if got := atomic.LoadInt64(&calls); got != want {
					t.Fatalf("upstream calls = %d, want %d", got, want)
				}
				rows, debited, desc := prepaidDebits(t, pool)
				if rows != debits || debited != int64(debits)*cost || desc != "shadow: AI call billing" {
					t.Fatalf("ledger = %d row(s), %d µLXC, %q; want %d, %d µLXC, %q",
						rows, debited, desc, debits, int64(debits)*cost, "shadow: AI call billing")
				}
			}

			ask(1, 1) // the model answers and the workspace keeps the answer
			ask(1, 1) // before the switch the repeat is replayed, free — the copy is there to be replayed
			var kept int
			if err := db.QueryRow(context.Background(), `SELECT count(*) FROM prompt_embeddings WHERE workspace_id = 'ws-log'`).Scan(&kept); err != nil {
				t.Fatal(err)
			}
			if kept == 0 {
				t.Fatal("no semantic copy was kept before the switch, so the repeat below cannot show it is not read")
			}

			if err := p.workspaceManager.SetLoggingPolicy(context.Background(), "ws-log", workspace.LoggingNone); err != nil {
				t.Fatal(err)
			}
			ask(2, 2) // after it: the model's answer, charged as a model call
		})
	}
}
