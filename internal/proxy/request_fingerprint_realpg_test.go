package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/migrations"
)

// B15.1 — A CACHED ANSWER IS SERVED ONLY TO A REQUEST ASKED UNDER THE SAME SETTINGS.
//
// Before B15.1 every cache key was the messages' text alone, so a question asked with a system
// prompt, a temperature or tools was answered with whatever the same words had been answered with
// under other settings — in one workspace, and across companies through the pool. This drives the
// real handler with a real exact cache (Redis) and a real semantic cache (pgvector) for the five
// cases the item names, on the exact path (the same words) and the semantic path (a rephrasing),
// asked again by the same workspace and by a second one through the pool.
//
// The embedder returns one vector for every text, so similarity is always 1.0: on the semantic path
// the ONLY thing that can turn a candidate away is the fingerprint (the prompts are chosen so the
// pool's entity gate passes them). A miss is proven by the upstream being called; a hit by it not
// being called AND the spend row naming the layer that served it.

type fpEmbedder struct{}

func (fpEmbedder) Model() string { return "text-embedding-3-small" }

func (fpEmbedder) Embed(context.Context, string) ([]float32, error) {
	v := make([]float32, 1536)
	v[0] = 1
	return v, nil
}

const (
	fpPrompt   = "How do I turn on logical replication in PostgreSQL 16?"
	fpRephrase = "In PostgreSQL 16, how do I turn on logical replication?"
)

// fpBase is the request every case varies by one field.
func fpBase(prompt string) map[string]string {
	return map[string]string{
		"model":      `"claude-haiku-4-5"`,
		"max_tokens": `256`,
		"messages":   `[{"role":"user","content":"` + prompt + `"}]`,
	}
}

func fpBody(fields map[string]string) string {
	parts := make([]string, 0, len(fields))
	for k, v := range fields {
		parts = append(parts, `"`+k+`":`+v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func fpDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG request fingerprint test")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_reqfp_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = ac.Close(ctx)
		t.Fatalf("create %s: %v", name, err)
	}
	_ = ac.Close(ctx)
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	mc, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		_ = mc.Close(ctx)
		t.Fatalf("migrate: %v", err)
	}
	_ = mc.Close(ctx)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if c, err := pgx.Connect(context.Background(), admin); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	return pool
}

func TestRequestFingerprint_EveryCacheLayerServesOnlyTheSameSettings(t *testing.T) {
	pool := fpDB(t)

	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant",`+
			`"content":[{"type":"text","text":"Set wal_level = logical."}],`+
			`"model":"claude-haiku-4-5","usage":{"input_tokens":20,"output_tokens":8}}`)
	}))
	t.Cleanup(up.Close)

	exact, redis := newExactCacheForTest(t)
	wsm := workspace.New(nil)
	for _, id := range []string{"wsA", "wsB"} {
		if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
			ID: id, Name: id, Active: true, LoggingPolicy: workspace.LoggingMetadata,
		}); err != nil {
			t.Fatal(err)
		}
		if err := wsm.SetCachePoolable(context.Background(), id, true); err != nil {
			t.Fatal(err)
		}
	}
	p := New(
		exact, cache.NewSemanticCache(pool, fpEmbedder{}, 0.9, time.Hour), fpEmbedder{},
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, wsm, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	p.router = nil // the served model stays the asked one
	p.anthropicURL = up.URL
	sink := &recordingAlertSink{}
	p.setAlertSink(sink)
	p.SetPoolGate(cache_pooling.New(func() bool { return true }, wsm.GetCachePoolable))

	send := func(ws string, fields map[string]string) (served string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(fpBody(fields)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		w := httptest.NewRecorder()
		before := atomic.LoadInt64(&calls)
		p.HandleAnthropic(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("ws=%s status=%d body=%s", ws, w.Code, w.Body.String())
		}
		if atomic.LoadInt64(&calls) != before {
			return "" // went to the model
		}
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.spends[len(sink.spends)-1].serveSource
	}

	cases := []struct {
		name   string
		change func(map[string]string)
		hit    bool
	}{
		{"identical except system", func(f map[string]string) { f["system"] = `"Answer only in French."` }, false},
		{"identical except temperature", func(f map[string]string) { f["temperature"] = `0.9` }, false},
		{"identical except tools", func(f map[string]string) {
			f["tools"] = `[{"name":"run_sql","description":"Run SQL","input_schema":{"type":"object"}}]`
		}, false},
		{"identical except stream", func(f map[string]string) { f["stream"] = `true` }, true},
		{"fully identical", func(map[string]string) {}, true},
	}
	paths := []struct {
		name, second string
		own, pooled  string // the layer that must serve a hit
	}{
		{"exact", fpPrompt, "cache_hit_exact", "cache_hit_pooled"},
		{"semantic", fpRephrase, "cache_hit_semantic", "cache_hit_pooled_semantic"},
	}
	askers := []struct{ name, ws string }{{"one workspace", "wsA"}, {"across two", "wsB"}}

	for _, path := range paths {
		for _, asker := range askers {
			for _, c := range cases {
				t.Run(path.name+"/"+asker.name+"/"+c.name, func(t *testing.T) {
					redis.FlushAll()
					if _, err := pool.Exec(context.Background(), `TRUNCATE prompt_embeddings`); err != nil {
						t.Fatal(err)
					}
					if got := send("wsA", fpBase(fpPrompt)); got != "" {
						t.Fatalf("the first ask was served from %q on an empty cache", got)
					}
					again := fpBase(path.second)
					c.change(again)
					want := ""
					if c.hit {
						want = path.own
						if asker.ws != "wsA" {
							want = path.pooled
						}
					}
					if got := send(asker.ws, again); got != want {
						t.Errorf("served from %q, want %q (\"\" = asked the model)", got, want)
					}
				})
			}
		}
	}
}
