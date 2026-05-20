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

// pgxDB is the subset of *pgxpool.Pool that SemanticCache needs.
// Defined so tests can substitute a pgxmock pool without a real database.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type SemanticCache struct {
	pool      pgxDB
	embedder  Embedder
	threshold float64
	ttl       time.Duration
}

func NewSemanticCache(pool *pgxpool.Pool, embedder Embedder, threshold float64, ttl time.Duration) *SemanticCache {
	return newSemanticCache(pool, embedder, threshold, ttl)
}

func newSemanticCache(pool pgxDB, embedder Embedder, threshold float64, ttl time.Duration) *SemanticCache {
	return &SemanticCache{pool: pool, embedder: embedder, threshold: threshold, ttl: ttl}
}

const semanticSelectSQL = `SELECT id, response, 1 - (embedding <=> $1) AS similarity
FROM prompt_embeddings
WHERE provider = $2 AND model = $3
  AND updated_at > NOW() - INTERVAL '24 hours'
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

