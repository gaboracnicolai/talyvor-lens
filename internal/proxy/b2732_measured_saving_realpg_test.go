package proxy

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/workspace"
)

// B27.32 — the saving Talyvor shows is measured. Four real requests through the proxy against real
// PG: one served at the model it asked for (saves nothing — the provider's own cache discount is in
// the baseline), one buffered and one streamed request routed to a cheaper model (the circuit
// breaker's downgrade), and an exact repeat served free from the cache. The month's saving — what the
// Spend screen shows — must equal the sum over the workspace's own rows of list − charged.
func TestB2732_TheMonthsSavingIsTheSumOfTheWorkspacesRows(t *testing.T) {
	pool := cacheVisPool(t)
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	const ws = "ws-b2732"
	if err := p.workspaceManager.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: ws, Name: "b2732", Active: true, LoggingPolicy: workspace.LoggingMetadata,
	}); err != nil {
		t.Fatal(err)
	}
	// The first request's spend opens this team's circuit, so every later live request is downgraded.
	am := alerts.New(pool, nil, []alerts.SpendRule{{ID: "b2732", Team: "b2732", Feature: "b2732",
		WindowHours: 24, WarningUSD: 1e9, CriticalUSD: 1e9, CircuitUSD: 1e-12}})
	p.setAlertSink(am)

	const usage = `"usage":{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":600}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"+
				"data: {\"choices\":[],"+usage+"}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}],`+usage+`}`)
	}))
	t.Cleanup(upstream.Close)
	p.openAIURL = upstream.URL

	send := func(question string, stream bool) {
		t.Helper()
		body := `{"model":"gpt-4o","messages":[{"role":"user","content":"` + question + `"}]}`
		if stream {
			body = `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"` + question + `"}]}`
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		req.Header.Set("X-Talyvor-Team", "b2732")
		req.Header.Set("X-Talyvor-Feature", "b2732")
		w := newFlushRecorder()
		p.HandleOpenAI(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%q: status %d, body=%s", question, w.Code, w.Body.String())
		}
	}
	send("what is one plus one", false)    // gpt-4o, as asked
	send("what is two plus two", false)    // routed to gpt-4o-mini
	send("what is three plus three", true) // streamed, routed to gpt-4o-mini
	send("what is one plus one", false)    // the exact repeat, from the cache

	rows, err := pool.Query(context.Background(), `
		SELECT requested_model, model, serve_source, list_cost_usd, charged_usd
		FROM token_events WHERE workspace_id = $1 ORDER BY created_at`, ws)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		asked, served, source string
		list, charged         float64
	}
	var got []row
	var sum float64
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.asked, &r.served, &r.source, &r.list, &r.charged); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
		sum += r.list - r.charged
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("rows = %d, want 4: %+v", len(got), got)
	}

	// 400 uncached + 600 cached input, 200 output — the provider's own breakdown, on both sides.
	routedSaving := alerts.CostUSDDetailed("gpt-4o", 400, 600, 0, 200) - alerts.CostUSDDetailed("gpt-4o-mini", 400, 600, 0, 200)
	asked, routed, streamed, repeat := got[0], got[1], got[2], got[3]
	if asked.served != "gpt-4o" || asked.list != asked.charged || asked.charged <= 0 {
		t.Errorf("served as asked: %+v, want gpt-4o with list == charged > 0 (no saving)", asked)
	}
	for _, r := range []row{routed, streamed} {
		if r.asked != "gpt-4o" || r.served != "gpt-4o-mini" || math.Abs(r.list-r.charged-routedSaving) > 1e-15 {
			t.Errorf("routed: %+v, want gpt-4o asked, gpt-4o-mini served, saving %.10f", r, routedSaving)
		}
	}
	if repeat.source != "cache_hit_exact" || repeat.charged != 0 || repeat.list <= 0 {
		t.Errorf("cache repeat: %+v, want cache_hit_exact charged 0 with a list price > 0", repeat)
	}

	month, err := am.MonthSaving(context.Background(), ws, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if month.Requests != 4 || month.Unmeasured != 0 {
		t.Errorf("month counts = %d measured / %d unmeasured, want 4 / 0", month.Requests, month.Unmeasured)
	}
	if month.SavedUSD <= 0 || math.Abs(month.SavedUSD-sum) > 1e-12 {
		t.Errorf("month saving = %.12f, want the rows' sum %.12f (> 0)", month.SavedUSD, sum)
	}
}
