package dbrouting

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const replicaPingTimeout = 5 * time.Second

// ReplicaOpts carries the primary's pool sizing + pooler topology so the
// replica pool is built the same way (notably: PgBouncer → simple protocol).
type ReplicaOpts struct {
	MaxConns  int32
	MinConns  int32
	PgBouncer bool
	Log       *slog.Logger
}

// OpenReplica builds the OPTIONAL read-replica pool from dsn. It returns nil —
// analytics reads then fall back to the primary via ReadPool — for EVERY
// failure mode: an empty dsn, a malformed dsn, a pool-init error, or a failed
// boot ping. Each failure logs a WARN rather than crashing: the replica is an
// optimization, never a boot dependency. When PgBouncer is set the replica
// uses simple protocol, mirroring the primary.
//
// Extracted from main.go so the boot-resilience fallback is unit-testable.
func OpenReplica(ctx context.Context, dsn string, opts ReplicaOpts) *pgxpool.Pool {
	if dsn == "" {
		return nil // unset → single-pool behavior, no WARN (this is the default)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		warn(opts.Log, "read-replica DSN invalid — analytics reads stay on primary", err)
		return nil
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	if opts.PgBouncer {
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		warn(opts.Log, "read-replica pool init failed — analytics reads stay on primary", err)
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, replicaPingTimeout)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		warn(opts.Log, "read-replica ping failed at boot — analytics reads stay on primary", err)
		pool.Close()
		return nil
	}
	if opts.Log != nil {
		opts.Log.Info("read-replica configured — analytics/display reads route to it; money/authz/tx stay on primary")
	}
	return pool
}

func warn(log *slog.Logger, msg string, err error) {
	if log != nil {
		log.Warn(msg, slog.String("err", err.Error()))
	}
}
