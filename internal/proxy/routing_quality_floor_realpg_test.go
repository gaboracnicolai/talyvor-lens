package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/routing"
	"github.com/talyvor/lens/migrations"
)

// B27.23: with cost-optimised routing on, the router wants gpt-4o → gpt-4o-mini for a simple question.
// The downgrade now stands only on measured quality: on a cohort where gpt-4o-mini's live-captured
// quality is below gpt-4o's, the named model answers and is billed; on one where it is at or above,
// gpt-4o-mini answers and the ledger row carries its price. Both seams — routing runs before the
// stream branches, so a stream is held or routed exactly as a buffered request is.
func TestQualityFloor_ACheaperModelAnswersOnlyWhereItMeasuresAtOrAboveTheNamedOne(t *testing.T) {
	named := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
	routed := settleULXC(alerts.CostUSD("gpt-4o-mini", 10000, 100))
	if routed >= named {
		t.Fatalf("fixture: gpt-4o-mini (%d µLXC) must cost less than gpt-4o (%d)", routed, named)
	}
	cases := []struct {
		feature, prompt string
		wantRouted      string // X-Talyvor-Routed, "" when held
		wantDebit       int64
	}{
		// Seeded below: gpt-4o-mini measures 0.70 here against gpt-4o's 0.92 — below the floor.
		{"support", "What is the capital of France?", "", named},
		// Seeded below: gpt-4o-mini measures 0.93 here against gpt-4o's 0.88 — at or above it.
		{"summaries", "What is the capital of Spain?", "gpt-4o→gpt-4o-mini", routed},
	}
	for _, stream := range []bool{false, true} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s/stream=%v", c.feature, stream), func(t *testing.T) {
				p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
				p.router = router.New() // chatProxy pins the served model; this test is about routing it
				ctx := context.Background()
				if err := p.workspaceManager.SetCostOptimizeRouting(ctx, "ws-log", true); err != nil {
					t.Fatalf("opt in: %v", err)
				}
				seedQualityCohorts(t, pool)
				gate := routing.NewQualityGate(mining.NewPatternMiner(nil, pool), routing.Config{})
				if err := gate.Refresh(ctx); err != nil {
					t.Fatalf("gate refresh: %v", err)
				}
				p.SetQualityGate(gate)
				var calls int64
				chatUpstream(t, p, stream, &calls) // reports 10,000 in / 100 out

				body := fmt.Sprintf(`{"model":"gpt-4o","stream":%v,"messages":[{"role":"user","content":%q}]}`, stream, c.prompt)
				req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Talyvor-Workspace", "ws-log")
				req.Header.Set("X-Talyvor-Feature", c.feature)
				req = req.WithContext(auth.WithAuthContext(req.Context(), sessionKeyAuthContext(t, "ws-log")))
				w := newFlushRecorder()
				p.HandleOpenAI(w, req)
				if w.Code != http.StatusOK || atomic.LoadInt64(&calls) != 1 {
					t.Fatalf("status = %d, upstream calls = %d; want 200 and 1", w.Code, calls)
				}
				if h := w.Header().Get("X-Talyvor-Routed"); h != c.wantRouted {
					t.Fatalf("X-Talyvor-Routed = %q, want %q", h, c.wantRouted)
				}
				rows, debited, desc := prepaidDebits(t, pool)
				if rows != 1 || debited != c.wantDebit {
					t.Fatalf("prepaid ledger = %d row(s), %d µLXC (%q); want 1 row of %d µLXC (gpt-4o %d, gpt-4o-mini %d)",
						rows, debited, desc, c.wantDebit, named, routed)
				}
			})
		}
	}
}

// seedQualityCohorts writes opted-in routing_patterns rows the way live capture does — one row per
// served request — 7 per model from each of 3 workspaces, so every model clears the 20-sample,
// 3-workspace floor and only the measured quality decides.
func seedQualityCohorts(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, f := range []string{"0023_patterns.sql", "0067_routing_patterns_complexity_bucket.sql"} {
		ddl, err := migrations.FS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM routing_patterns`); err != nil {
		t.Fatal(err)
	}
	quality := map[string]map[string]float64{
		"support":   {"gpt-4o": 0.92, "gpt-4o-mini": 0.70},
		"summaries": {"gpt-4o": 0.88, "gpt-4o-mini": 0.93},
	}
	small := mining.InputBucketFor(10) // both prompts are a few tokens
	for feature, byModel := range quality {
		for model, q := range byModel {
			for _, ws := range []string{"ws-a", "ws-b", "ws-c"} {
				for i := 0; i < 7; i++ {
					if _, err := pool.Exec(ctx, `INSERT INTO routing_patterns
						(workspace_id, feature_category, model_used, provider_used, input_token_range, output_quality, latency_bucket, opted_in)
						VALUES ($1, $2, $3, 'openai', $4, $5, 'fast', TRUE)`, ws, feature, model, small, q); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
}
