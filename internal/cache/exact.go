package cache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrCacheMiss = errors.New("cache miss")

type ExactCache struct {
	client *redis.Client
	ttl    time.Duration
}

func NewExactCache(client *redis.Client, ttl time.Duration) *ExactCache {
	return &ExactCache{client: client, ttl: ttl}
}

func (c *ExactCache) Get(ctx context.Context, key string) ([]byte, error) {
	b, err := c.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrCacheMiss
	}
	return b, err
}

func (c *ExactCache) Set(ctx context.Context, key string, value []byte) error {
	return c.client.Set(ctx, key, value, c.ttl).Err()
}
