package reqtrack

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
)

// A request in a Redis command reads "redis", and goes back to what it was doing after.
func TestRedisHookMarksTheRequestRedis(t *testing.T) {
	q := &Request{}
	ctx := context.WithValue(context.Background(), ctxKey{}, q)
	var during Phase
	err := RedisHook{}.ProcessHook(func(ctx context.Context, _ redis.Cmder) error {
		during = FromContext(ctx).Phase()
		return nil
	})(ctx, nil)
	if err != nil || during != Redis || q.Phase() != Handler {
		t.Fatalf("during %v, after %v, err %v", during, q.Phase(), err)
	}
}
