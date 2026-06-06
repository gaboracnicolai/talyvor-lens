package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/metrics"
)

// Embedder turns a text prompt into an embedding vector.
// Implemented in other packages (e.g. an OpenAI-backed embedder).
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// SemanticDB is the subset of *pgxpool.Pool that SemanticCache needs. Exported
// so tests (including in other packages, e.g. the proxy) can substitute a
// pgxmock pool without a real database.
type SemanticDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type SemanticCache struct {
	pool      SemanticDB
	embedder  Embedder
	threshold float64
	ttl       time.Duration
}

func NewSemanticCache(pool *pgxpool.Pool, embedder Embedder, threshold float64, ttl time.Duration) *SemanticCache {
	return newSemanticCache(pool, embedder, threshold, ttl)
}

// NewSemanticCacheWithDB builds a SemanticCache over any SemanticDB (e.g. a
// pgxmock pool in tests).
func NewSemanticCacheWithDB(pool SemanticDB, embedder Embedder, threshold float64, ttl time.Duration) *SemanticCache {
	return newSemanticCache(pool, embedder, threshold, ttl)
}

func newSemanticCache(pool SemanticDB, embedder Embedder, threshold float64, ttl time.Duration) *SemanticCache {
	return &SemanticCache{pool: pool, embedder: embedder, threshold: threshold, ttl: ttl}
}

// semanticSelectSQL is the PRIVATE (workspace-scoped) lookup. The
// `is_poolable = false` filter excludes shared-pool rows so a private lookup can
// never serve a pooled entry (which would bypass the cross-tenant consent
// check). It is a no-op when pooling is off: every row is is_poolable=false by
// default, so the result set is identical to before Stage 2.0b.
const semanticSelectSQL = `SELECT id, response, 1 - (embedding <=> $1) AS similarity
FROM prompt_embeddings
WHERE provider = $2 AND model = $3
  AND updated_at > NOW() - INTERVAL '24 hours'
  AND is_poolable = false
ORDER BY embedding <=> $1
LIMIT 1`

// semanticSelectPooledSQL is the SHARED-POOL lookup: it ranges ONLY over
// is_poolable=true rows and returns the contributing workspace so the caller can
// verify the contributor's live opt-in. COALESCE makes a missing contributor an
// empty string (→ the gate blocks it).
const semanticSelectPooledSQL = `SELECT id, response, COALESCE(contributor_workspace_id, '') AS contributor, 1 - (embedding <=> $1) AS similarity
FROM prompt_embeddings
WHERE provider = $2 AND model = $3
  AND updated_at > NOW() - INTERVAL '24 hours'
  AND is_poolable = true
ORDER BY embedding <=> $1
LIMIT 1`

const semanticTouchSQL = `UPDATE prompt_embeddings
SET hit_count = hit_count + 1, updated_at = NOW()
WHERE id = $1`

const semanticUpsertSQL = `INSERT INTO prompt_embeddings
  (provider, model, prompt_hash, embedding, response)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (prompt_hash) DO UPDATE SET
  response = EXCLUDED.response,
  embedding = EXCLUDED.embedding,
  updated_at = NOW()`

// semanticUpsertPooledSQL writes a shared-pool row: contributor stamped,
// is_poolable=true (a literal). Its prompt_hash is keyed on a NUL-sentinel-
// prefixed prompt (the caller's job), provably disjoint from any private hash.
const semanticUpsertPooledSQL = `INSERT INTO prompt_embeddings
  (provider, model, prompt_hash, embedding, response, contributor_workspace_id, is_poolable)
VALUES ($1, $2, $3, $4, $5, $6, true)
ON CONFLICT (prompt_hash) DO UPDATE SET
  response = EXCLUDED.response,
  embedding = EXCLUDED.embedding,
  contributor_workspace_id = EXCLUDED.contributor_workspace_id,
  is_poolable = true,
  updated_at = NOW()`

func (c *SemanticCache) Get(ctx context.Context, provider, model, prompt string) ([]byte, error) {
	vec, err := c.embedder.Embed(ctx, prompt)
	if err != nil {
		return nil, err
	}

	var (
		id         string
		response   string
		similarity float64
	)
	err = c.pool.QueryRow(ctx, semanticSelectSQL, vectorLiteral(vec), provider, model).
		Scan(&id, &response, &similarity)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if similarity < c.threshold {
		return nil, nil
	}

	if _, err := c.pool.Exec(ctx, semanticTouchSQL, id); err != nil {
		return nil, err
	}

	metrics.CacheHitsTotal.WithLabelValues("semantic").Inc()
	return []byte(response), nil
}

func (c *SemanticCache) Set(ctx context.Context, provider, model, prompt string, response []byte, embedding []float32) error {
	sum := sha256.Sum256([]byte(provider + ":" + model + ":" + prompt))
	hash := hex.EncodeToString(sum[:])

	_, err := c.pool.Exec(
		ctx,
		semanticUpsertSQL,
		provider, model, hash, vectorLiteral(embedding), string(response),
	)
	return err
}

// SetPooled writes a SHARED-POOL row (is_poolable=true) tagged with the
// contributing workspace. The caller supplies a `prompt` already prefixed with
// the NUL-sentinel pooled marker, so the row's prompt_hash is provably disjoint
// from any workspace-private hash (which carries a "wsID:" prefix). Used only on
// the opt-in path — Stage 2.0b's cross-tenant write surface.
func (c *SemanticCache) SetPooled(ctx context.Context, provider, model, prompt, contributorWsID string, response []byte, embedding []float32) error {
	sum := sha256.Sum256([]byte(provider + ":" + model + ":" + prompt))
	hash := hex.EncodeToString(sum[:])

	_, err := c.pool.Exec(
		ctx,
		semanticUpsertPooledSQL,
		provider, model, hash, vectorLiteral(embedding), string(response), contributorWsID,
	)
	return err
}

// GetPooled is the cross-tenant similarity lookup: it searches ONLY is_poolable
// rows and returns the cached response, the contributing workspace, the matched
// row's prompt_embeddings.id, and the similarity score. A miss (no row, or
// below threshold) is (nil, "", "", 0, nil). The contributor lets the caller
// verify the contributor's live opt-in before serving; an empty contributor
// (defensive — should not occur for a poolable row) surfaces as "" so the gate
// blocks it. The entry id + similarity are Stage-2.1 attribution data for the
// royalty claim row — NOT an idempotency key (a retried request can re-match a
// different row: ORDER BY similarity LIMIT 1 over a moving 24h window).
func (c *SemanticCache) GetPooled(ctx context.Context, provider, model, prompt string) ([]byte, string, string, float64, error) {
	vec, err := c.embedder.Embed(ctx, prompt)
	if err != nil {
		return nil, "", "", 0, err
	}

	var (
		id          string
		response    string
		contributor string
		similarity  float64
	)
	err = c.pool.QueryRow(ctx, semanticSelectPooledSQL, vectorLiteral(vec), provider, model).
		Scan(&id, &response, &contributor, &similarity)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", "", 0, nil
	}
	if err != nil {
		return nil, "", "", 0, err
	}
	if similarity < c.threshold {
		return nil, "", "", 0, nil
	}
	if _, err := c.pool.Exec(ctx, semanticTouchSQL, id); err != nil {
		return nil, "", "", 0, err
	}

	metrics.CacheHitsTotal.WithLabelValues("semantic_pooled").Inc()
	return []byte(response), contributor, id, similarity, nil
}

// vectorLiteral encodes a vector in pgvector's text format: "[v1,v2,...]".
func vectorLiteral(v []float32) string {
	var sb strings.Builder
	sb.Grow(len(v)*8 + 2)
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}
