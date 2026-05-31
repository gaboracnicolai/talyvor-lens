package ha

import (
	"context"
	"log/slog"
	"time"
)

const leaderKeyPrefix = "ha:leader:"

// renewLua refreshes the TTL only if this instance still holds the key.
// Returns 1 on success, 0 if the key is gone or owned by another instance.
const renewLua = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("PEXPIRE", KEYS[1], ARGV[2])
else
    return 0
end`

// releaseLua deletes the key only if this instance still holds it.
// Prevents a slow instance from evicting a lock another instance has re-acquired.
const releaseLua = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
else
    return 0
end`

// Leader provides distributed leader election for singleton background jobs.
// Only one instance across the cluster runs the job at a time; if it crashes,
// the Redis key expires within ttl and another instance takes over.
//
// When HA is disabled fn is called directly — single-instance behaviour is
// byte-for-byte unchanged.
type Leader struct {
	rdb     redisClient
	selfID  string
	enabled bool
}

// NewLeader builds a Leader. rdb may be nil when disabled.
func NewLeader(rdb redisClient, selfID string, enabled bool) *Leader {
	return &Leader{rdb: rdb, selfID: selfID, enabled: enabled}
}

func (l *Leader) key(job string) string { return leaderKeyPrefix + job }

// Run acquires exclusive leadership for job and calls fn(ctx) while holding
// it. ttl is both the lock expiry and the failover window after a crash.
// The lock renews every ttl/3; if renewal fails fn's context is cancelled and
// Run loops back to re-acquire. Run blocks until ctx is done.
func (l *Leader) Run(ctx context.Context, job string, ttl time.Duration, fn func(context.Context)) {
	if !l.enabled {
		fn(ctx)
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		ok, err := l.rdb.SetNX(ctx, l.key(job), l.selfID, ttl).Result()
		if err != nil {
			slog.Warn("ha: leader acquire error", slog.String("job", job), slog.String("err", err.Error()))
		}
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-time.After(ttl / 2):
				continue
			}
		}
		slog.Info("ha: acquired leadership", slog.String("job", job), slog.String("instance", l.selfID))
		l.runAsLeader(ctx, job, ttl, fn)
		// brief pause before trying to re-acquire after fn exits or leadership lost
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (l *Leader) runAsLeader(ctx context.Context, job string, ttl time.Duration, fn func(context.Context)) {
	fnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(fnCtx)
	}()

	tick := time.NewTicker(ttl / 3)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			l.release(context.Background(), job)
			<-done
			return
		case <-done:
			l.release(ctx, job)
			return
		case <-tick.C:
			held, err := l.renew(ctx, job, ttl)
			if err != nil {
				slog.Warn("ha: leader renew error", slog.String("job", job), slog.String("err", err.Error()))
			}
			if !held {
				slog.Warn("ha: lost leadership", slog.String("job", job), slog.String("instance", l.selfID))
				cancel()
				<-done
				return
			}
		}
	}
}

func (l *Leader) renew(ctx context.Context, job string, ttl time.Duration) (bool, error) {
	res, err := l.rdb.Eval(ctx, renewLua, []string{l.key(job)}, l.selfID, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

func (l *Leader) release(ctx context.Context, job string) {
	if _, err := l.rdb.Eval(ctx, releaseLua, []string{l.key(job)}, l.selfID).Result(); err != nil {
		slog.Warn("ha: leader release failed", slog.String("job", job), slog.String("err", err.Error()))
	}
}
