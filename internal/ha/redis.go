package ha

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisClient is the subset of *redis.Client that the HA package depends on.
// The repo has no pre-existing Redis interface — code uses *redis.Client
// directly — so this declaration is the reuse seam: *redis.Client satisfies it
// unchanged, and it documents exactly which Redis operations HA needs (which is
// also the contract a future fake would have to honour). It embeds
// redis.Scripter so the shared limiter can run its Lua script through it.
type redisClient interface {
	redis.Scripter
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.BoolCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	Scan(ctx context.Context, cursor uint64, match string, count int64) *redis.ScanCmd
	Ping(ctx context.Context) *redis.StatusCmd
	Publish(ctx context.Context, channel string, message interface{}) *redis.IntCmd
	Subscribe(ctx context.Context, channels ...string) *redis.PubSub
}
