package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/templates"
	"github.com/talyvor/lens/internal/workspace"
)

// B18.4 — "logging: none" MEANS NOTHING IS STORED.
//
// Decided 27 Sep: a workspace whose logging_policy is none has no prompt or response content stored
// — no exact cache, no semantic cache, no pool contribution, no content logs — and may still be
// served from the shared pool, because reading stores nothing.
//
// Through the real handler, with the content stores it writes to wired: the exact cache (Redis), the
// semantic cache and the prompt-template recorder (a fully migrated Postgres). After the none
// workspace asks a question carrying a unique marker in its system prompt and its question, and gets
// an answer carrying it too, the marker is searched for in EVERY text, json and array column of
// every table in that database, and in every Redis key and value: zero. Then another workspace
// contributes an answer and the none workspace is served it from the pool, writing nothing.
func TestB184_LoggingNoneStoresNoContentAndIsStillServedFromThePool(t *testing.T) {
	p, _, _ := chatCacheProxy(t, "")
	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		last := req.Messages[len(req.Messages)-1].Content
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":`+
			jsonString("ANSWER: "+last[strings.LastIndex(last, "\n")+1:])+`}],"model":"claude-haiku-4-5","usage":{"input_tokens":16,"output_tokens":9}}`)
	}))
	t.Cleanup(up.Close)
	p.anthropicURL = up.URL

	db := fpDB(t)
	p.semantic, p.embedder = cache.NewSemanticCache(db, fpEmbedder{}, 0.98, time.Hour), fpEmbedder{}
	exact, redis := newExactCacheForTest(t)
	p.exact = exact
	p.templateDetector = templates.New(db)
	const none = "wsPoolB"
	if err := p.workspaceManager.SetLoggingPolicy(context.Background(), none, workspace.LoggingNone); err != nil {
		t.Fatal(err)
	}

	ask := func(ws, system, question string) string {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"model": "claude-haiku-4-5", "max_tokens": 256, "system": system,
			"messages": []map[string]string{{"role": "user", "content": question}}})
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
		w := httptest.NewRecorder()
		p.HandleAnthropic(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("ws=%s status=%d body=%s", ws, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	// found lists every place the marker is stored: each text, json or array column of each table,
	// and each Redis key and value.
	found := func(marker string) []string {
		t.Helper()
		ctx := context.Background()
		rows, err := db.Query(ctx, `SELECT table_name, column_name FROM information_schema.columns
			WHERE table_schema = current_schema() AND data_type IN ('text', 'character varying', 'jsonb', 'json', 'ARRAY')`)
		if err != nil {
			t.Fatal(err)
		}
		var cols [][2]string
		for rows.Next() {
			var tbl, col string
			if err := rows.Scan(&tbl, &col); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, [2]string{tbl, col})
		}
		rows.Close()
		if len(cols) < 100 {
			t.Fatalf("only %d text columns to search — the scan is blind", len(cols))
		}
		var hits []string
		for _, c := range cols {
			var n int
			q := fmt.Sprintf(`SELECT count(*) FROM %q WHERE %q::text LIKE '%%' || $1 || '%%'`, c[0], c[1])
			if err := db.QueryRow(ctx, q, marker).Scan(&n); err != nil {
				t.Fatalf("%s.%s: %v", c[0], c[1], err)
			}
			if n > 0 {
				hits = append(hits, fmt.Sprintf("%s.%s (%d rows)", c[0], c[1], n))
			}
		}
		for _, k := range redis.Keys() {
			v, _ := redis.Get(k)
			if strings.Contains(k, marker) || strings.Contains(v, marker) {
				hits = append(hits, "redis "+k)
			}
		}
		return hits
	}

	// A control: the same question from a workspace that DOES store is found by the same scan.
	ask("wsPoolA", "You are a helpful assistant. ctrl-marker-9f3a", "What is the capital of France? ctrl-marker-9f3a")
	if hits := found("ctrl-marker-9f3a"); len(hits) == 0 {
		t.Fatal("a metadata-logging workspace's content was not found either — the scan finds nothing, so it proves nothing")
	}

	// The none workspace's own question: answered, and stored nowhere.
	got := ask(none, "You are a helpful assistant. none-marker-7c1e", "What is the capital of Italy? none-marker-7c1e")
	if !strings.Contains(got, "none-marker-7c1e") {
		t.Fatalf("the answer does not carry the marker, so the scan would not see it stored: %s", got)
	}
	if hits := found("none-marker-7c1e"); len(hits) > 0 {
		t.Errorf("a logging_policy = none workspace's content is stored in: %s", strings.Join(hits, ", "))
	}

	// And it is still served from the pool: another workspace's answer, reused, writing nothing.
	const system, shared = "You are a helpful assistant.", "What is the capital of Spain?"
	ask("wsPoolA", system, shared)
	before := atomic.LoadInt64(&calls)
	if got := ask(none, system, shared); atomic.LoadInt64(&calls) != before || !strings.Contains(got, "ANSWER: "+shared) {
		t.Errorf("the none workspace was not served the pooled answer: calls %d→%d, body %s", before, atomic.LoadInt64(&calls), got)
	}
	var rows int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM prompt_embeddings
		WHERE workspace_id = $1 OR contributor_workspace_id = $1`, none).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("the none workspace has %d prompt_embeddings rows after a pooled serve, want 0", rows)
	}
	for _, k := range redis.Keys() {
		if v, _ := redis.Get(k); v == none {
			t.Errorf("an exact-cache entry is owned by the none workspace: %s", k)
		}
	}
}
