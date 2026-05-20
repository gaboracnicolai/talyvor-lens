package cache

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type SemanticCache struct {
	pool      *pgxpool.Pool
	threshold float64
}

func NewSemanticCache(pool *pgxpool.Pool, threshold float64) *SemanticCache {
	return &SemanticCache{pool: pool, threshold: threshold}
}

type Match struct {
	Response    string
	Similarity  float64
	TokensSaved int
}

func (s *SemanticCache) Lookup(ctx context.Context, embedding []float32) (*Match, error) {
	_ = ctx
	_ = embedding
	return nil, ErrCacheMiss
}

func (s *SemanticCache) Store(ctx context.Context, provider, model, promptHash, response string, embedding []float32) error {
	_ = ctx
	_ = provider
	_ = model
	_ = promptHash
	_ = response
	_ = embedding
	return nil
}
