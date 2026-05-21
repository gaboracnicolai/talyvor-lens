package templates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pinAtHits is the hit threshold at which a template gets pinned and the
// caller should start applying provider-native prompt caching.
const pinAtHits = 10

// pgxDB is the subset of *pgxpool.Pool that the detector needs. Tests pass
// nil so they exercise only the in-memory hot path.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type TemplateDetector struct {
	pool      pgxDB
	mu        sync.RWMutex
	templates map[string]*Template
}

type Template struct {
	Hash       string
	Content    string
	HitCount   int
	PinnedAt   *time.Time
	Provider   string
	TokenCount int
}

func New(pool *pgxpool.Pool) *TemplateDetector {
	// Avoid the typed-nil interface trap: a (*pgxpool.Pool)(nil) stored
	// in a pgxDB interface compares != nil but panics on call.
	if pool == nil {
		return newDetector(nil)
	}
	return newDetector(pool)
}

func newDetector(pool pgxDB) *TemplateDetector {
	return &TemplateDetector{
		pool:      pool,
		templates: make(map[string]*Template),
	}
}

// extractRequest is loose enough to cover both OpenAI and Anthropic shapes
// in a single Unmarshal: Anthropic puts the system prompt at top level,
// OpenAI puts it as messages[0] with role=system.
type extractRequest struct {
	System   json.RawMessage `json:"system"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func (d *TemplateDetector) ExtractSystemPrompt(body []byte) (string, bool) {
	var req extractRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", false
	}

	// Anthropic-style top-level "system" — may be a string or an array of
	// content blocks. We accept either; for arrays, we concatenate the
	// "text" fields so the hash is stable across equivalent forms.
	if len(req.System) > 0 {
		var s string
		if err := json.Unmarshal(req.System, &s); err == nil && s != "" {
			return s, true
		}
		var blocks []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(req.System, &blocks); err == nil && len(blocks) > 0 {
			var combined string
			for _, b := range blocks {
				combined += b.Text
			}
			if combined != "" {
				return combined, true
			}
		}
	}

	// OpenAI-style: messages[0] with role=="system".
	for _, m := range req.Messages {
		if m.Role != "system" {
			continue
		}
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil && s != "" {
			return s, true
		}
		// Some clients send content as an array of content blocks even for
		// chat-completions; flatten for hashing.
		var blocks []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err == nil && len(blocks) > 0 {
			var combined string
			for _, b := range blocks {
				combined += b.Text
			}
			if combined != "" {
				return combined, true
			}
		}
		break // only the first system message
	}

	return "", false
}

func hashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

const insertTemplateSQL = `INSERT INTO prompt_templates (hash, content, provider, token_count)
VALUES ($1, $2, $3, $4)
ON CONFLICT (hash) DO NOTHING`

const incrementHitSQL = `UPDATE prompt_templates SET hit_count = hit_count + 1, updated_at = NOW()
WHERE hash = $1`

const markPinnedSQL = `UPDATE prompt_templates SET pinned_at = NOW(), updated_at = NOW()
WHERE hash = $1 AND pinned_at IS NULL`

// RecordAndPin updates the in-memory hit counter and writes through to the
// database on a best-effort basis. The in-memory map is the source of truth
// for the hot path; the DB is an append-only audit log so newly-started
// instances can re-hydrate counters in a future iteration.
//
// Returns the template plus pinned=true exactly once — on the call that
// crosses the pin threshold. Callers should apply provider-native caching
// when pinned is true.
func (d *TemplateDetector) RecordAndPin(ctx context.Context, systemPrompt, provider string) (*Template, bool) {
	hash := hashContent(systemPrompt)

	d.mu.Lock()
	tmpl, exists := d.templates[hash]
	if !exists {
		tmpl = &Template{
			Hash:       hash,
			Content:    systemPrompt,
			Provider:   provider,
			HitCount:   1,
			TokenCount: len(systemPrompt) / 4, // same len/4 estimate as the rest of the pipeline
		}
		d.templates[hash] = tmpl
		d.mu.Unlock()
		d.tryInsert(ctx, tmpl)
		return tmpl, false
	}
	tmpl.HitCount++

	var justPinned bool
	if tmpl.HitCount >= pinAtHits && tmpl.PinnedAt == nil {
		now := time.Now().UTC()
		tmpl.PinnedAt = &now
		justPinned = true
	}
	d.mu.Unlock()

	d.tryIncrement(ctx, hash)
	if justPinned {
		d.tryMarkPinned(ctx, hash)
	}
	return tmpl, justPinned
}

func (d *TemplateDetector) tryInsert(ctx context.Context, t *Template) {
	if d.pool == nil {
		return
	}
	if _, err := d.pool.Exec(ctx, insertTemplateSQL, t.Hash, t.Content, t.Provider, t.TokenCount); err != nil {
		slog.Warn("templates: INSERT failed", slog.String("err", err.Error()))
	}
}

func (d *TemplateDetector) tryIncrement(ctx context.Context, hash string) {
	if d.pool == nil {
		return
	}
	if _, err := d.pool.Exec(ctx, incrementHitSQL, hash); err != nil {
		slog.Warn("templates: UPDATE hit_count failed", slog.String("err", err.Error()))
	}
}

func (d *TemplateDetector) tryMarkPinned(ctx context.Context, hash string) {
	if d.pool == nil {
		return
	}
	if _, err := d.pool.Exec(ctx, markPinnedSQL, hash); err != nil {
		slog.Warn("templates: UPDATE pinned_at failed", slog.String("err", err.Error()))
	}
}

// ApplyOpenAICaching is intentionally a no-op. OpenAI's API enables prompt
// caching automatically for prompts over 1024 tokens — the value of template
// detection here is observability, not modifying the wire format. Provider-
// native caching kicks in as long as the system message stays first and
// byte-identical, which is what our cache key already guarantees.
func (d *TemplateDetector) ApplyOpenAICaching(body []byte, _ *Template) ([]byte, error) {
	return body, nil
}

// ApplyAnthropicCaching rewrites the request body to mark the system prompt
// with cache_control: ephemeral, which is how Anthropic opts a span into
// its prompt-caching feature. A string-form system field is upgraded to the
// array form; an array-form system gets cache_control added to its LAST
// element so multi-block system prompts are cached in full.
func (d *TemplateDetector) ApplyAnthropicCaching(body []byte, _ *Template) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, err
	}

	switch v := m["system"].(type) {
	case string:
		m["system"] = []any{
			map[string]any{
				"type":          "text",
				"text":          v,
				"cache_control": map[string]any{"type": "ephemeral"},
			},
		}
	case []any:
		if len(v) == 0 {
			return body, nil
		}
		last, ok := v[len(v)-1].(map[string]any)
		if !ok {
			return body, nil
		}
		last["cache_control"] = map[string]any{"type": "ephemeral"}
	default:
		// No system field — nothing to cache, nothing to rewrite.
		return body, nil
	}

	out, err := json.Marshal(m)
	if err != nil {
		return body, err
	}
	return out, nil
}
