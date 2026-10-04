package reqtrack

import (
	"context"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// PgxTracer marks a request db_acquire while pgxpool waits for a free connection and db_query while a
// query, batch or copy runs. Set it as pgxpool.Config.ConnConfig.Tracer; pgxpool picks up the acquire
// half from there.
type PgxTracer struct{}

type markKey struct{}

func enterInto(ctx context.Context, p Phase) context.Context {
	if FromContext(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, markKey{}, Enter(ctx, p))
}

func exitFrom(ctx context.Context) {
	if m, ok := ctx.Value(markKey{}).(Mark); ok {
		m.Exit()
	}
}

func (PgxTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return enterInto(ctx, DBAcquire)
}

func (PgxTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireEndData) {
	exitFrom(ctx)
}

func (PgxTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return enterInto(ctx, DBQuery)
}

func (PgxTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	exitFrom(ctx)
}

func (PgxTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return enterInto(ctx, DBQuery)
}

func (PgxTracer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (PgxTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchEndData) {
	exitFrom(ctx)
}

func (PgxTracer) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceCopyFromStartData) context.Context {
	return enterInto(ctx, DBQuery)
}

func (PgxTracer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceCopyFromEndData) {
	exitFrom(ctx)
}

// RedisHook marks a request redis while a command or pipeline runs. Install with Client.AddHook.
type RedisHook struct{}

func (RedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (RedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		defer Enter(ctx, Redis).Exit()
		return next(ctx, cmd)
	}
}

func (RedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		defer Enter(ctx, Redis).Exit()
		return next(ctx, cmds)
	}
}

// Transport marks a request upstream while a round trip to the provider is in flight and while its
// response body is being read — which is where a streamed completion waits. A nil base is
// http.DefaultTransport.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return transport{base}
}

type transport struct{ base http.RoundTripper }

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	q := FromContext(req.Context())
	if q == nil {
		return t.base.RoundTrip(req)
	}
	m := Enter(req.Context(), Upstream)
	resp, err := t.base.RoundTrip(req)
	m.Exit()
	if err == nil && resp.Body != nil {
		resp.Body = upstreamBody{resp.Body, req.Context()}
	}
	return resp, err
}

type upstreamBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b upstreamBody) Read(p []byte) (int, error) {
	defer Enter(b.ctx, Upstream).Exit()
	return b.ReadCloser.Read(p)
}

// Pool is the "database_pool" section of /healthz. WaitCount is how many acquires found no idle
// connection and had to wait; WaitMs is the total time they waited; CanceledWaits gave up waiting.
type Pool struct {
	InUse         int32 `json:"in_use"`
	Idle          int32 `json:"idle"`
	Max           int32 `json:"max"`
	WaitCount     int64 `json:"wait_count"`
	WaitMs        int64 `json:"wait_ms"`
	CanceledWaits int64 `json:"canceled_waits"`
}

// PoolStats reads p's counters.
func PoolStats(p *pgxpool.Pool) Pool {
	s := p.Stat()
	return Pool{
		InUse:         s.AcquiredConns(),
		Idle:          s.IdleConns(),
		Max:           s.MaxConns(),
		WaitCount:     s.EmptyAcquireCount(),
		WaitMs:        s.EmptyAcquireWaitTime().Milliseconds(),
		CanceledWaits: s.CanceledAcquireCount(),
	}
}
