package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/migrations"
)

// B15.3, on the ledger: a browser-chat stream from a workspace that turned on cost-optimised routing is
// answered by the cheaper model AND charged at the cheaper model's price — the saving reaches the
// prepaid ledger row, not only the model field.
func TestStreamSavings_ARoutedChatStreamIsChargedAtTheModelThatAnswered(t *testing.T) {
	p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
	p.router = router.New() // chatProxy pins the served model; this test is about routing it
	if err := p.workspaceManager.SetCostOptimizeRouting(context.Background(), "ws-log", true); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	var calls int64
	chatUpstream(t, p, true, &calls) // reports 10,000 in / 100 out

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"What is the capital of France?"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-log")
	req = req.WithContext(auth.WithAuthContext(req.Context(), sessionKeyAuthContext(t, "ws-log")))
	w := newFlushRecorder()
	p.HandleOpenAI(w, req)
	if w.Code != http.StatusOK || atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("status = %d, upstream calls = %d; want 200 and 1", w.Code, calls)
	}
	if h := w.Header().Get("X-Talyvor-Routed"); h != "gpt-4o→gpt-4o-mini" {
		t.Fatalf("X-Talyvor-Routed = %q — the stream was not routed, so the ledger below proves nothing", h)
	}

	routed := settleULXC(alerts.CostUSD("gpt-4o-mini", 10000, 100))
	named := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
	if routed >= named {
		t.Fatalf("fixture: gpt-4o-mini (%d µLXC) must cost less than gpt-4o (%d) for this to show a saving", routed, named)
	}
	rows, debited, desc := prepaidDebits(t, pool)
	if rows != 1 || debited != routed {
		t.Fatalf("prepaid ledger = %d row(s), %d µLXC (%q); want 1 row of %d µLXC — gpt-4o-mini's price, not gpt-4o's %d",
			rows, debited, desc, routed, named)
	}
}

// A stream writes its per-request attribution row — the one the IDE dashboard and the issue's cost read —
// naming the model that answered, as a buffered request does. It had none: streamed requests were absent
// from request_attribution entirely.
//
// The table lives in its own migrated database, never the shared lens_test (see
// internal/attribution's w639Store for why migrating lens_test breaks other packages).
func TestStreamSavings_AStreamWritesItsAttributionRow(t *testing.T) {
	dsn := os.Getenv("LENS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("LENS_TEST_DATABASE_URL not set — this test must RUN, not skip")
	}
	ctx := context.Background()
	const db = "lens_b153_stream_attribution"
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+db); err != nil && !strings.Contains(err.Error(), "already exists") {
		admin.Close()
		t.Fatalf("create %s: %v", db, err)
	}
	admin.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, conn.Conn(), migrations.FS); err != nil {
		conn.Release()
		t.Fatalf("migrate: %v", err)
	}
	conn.Release()

	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	up := newStreamEcho(t, p)
	p.SetAttributionStore(attribution.NewStore(pool))
	if err := p.workspaceManager.SetCostOptimizeRouting(ctx, "ws-log", true); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	requestID := "b153-attr-" + time.Now().Format("150405.000000")
	streamChat(t, p, "gpt-4o", `"What is the capital of France?"`, map[string]string{"X-Talyvor-Request-ID": requestID})
	if got := up.sentModel(t, 0); got != "gpt-4o-mini" {
		t.Fatalf("the stream was sent %q, want the routed gpt-4o-mini", got)
	}

	var model string
	var in, out int
	var cost float64
	deadline := time.Now().Add(5 * time.Second) // RecordAsync writes off the request path
	for {
		err = pool.QueryRow(ctx, `SELECT model, input_tokens, output_tokens, cost_usd FROM request_attribution
			WHERE request_id = $1`, requestID).Scan(&model, &in, &out, &cost)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("no request_attribution row for the streamed request %s: %v", requestID, err)
	}
	if model != "gpt-4o-mini" || in != 1200 || out != 40 || cost != alerts.CostUSD("gpt-4o-mini", 1200, 40) {
		t.Fatalf("attribution row = %s %d in / %d out $%v; want gpt-4o-mini 1200 / 40 $%v",
			model, in, out, cost, alerts.CostUSD("gpt-4o-mini", 1200, 40))
	}
}
