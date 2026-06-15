package dbrouting

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// replicaLagSQL returns the standby's replay lag in seconds. It is 0 when the
// queried node is NOT in recovery (a primary, or a replica DSN that actually
// points at the primary) and 0 when a freshly-promoted standby has not yet
// replayed a transaction (NULL timestamp → COALESCE 0). Safe to run on either
// a primary or a standby.
const replicaLagSQL = `SELECT COALESCE(
	CASE WHEN pg_is_in_recovery()
	     THEN EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp()))
	     ELSE 0 END, 0)`

const (
	lagSampleInterval = 10 * time.Second
	lagSampleTimeout  = 5 * time.Second
	lagCheckTimeout   = 100 * time.Millisecond
)

// LagMonitor periodically samples read-replica replay lag and publishes it via
// the injected setter (wired to the Prometheus gauge in main.go). It also
// satisfies the health-checker contract (Check). Both are no-ops when no
// replica is configured, so the gauge stays 0 and the health entry reports a
// healthy "no replica configured". Decoupled from the metrics/api packages so
// dbrouting stays dependency-light; the LENS_DB_PGBOUNCER simple-protocol
// setup is inherited from the pool main.go hands in.
type LagMonitor struct {
	replica  *pgxpool.Pool
	publish  func(float64)
	interval time.Duration
	log      *slog.Logger
}

// NewLagMonitor builds a monitor for the given replica pool (nil → a no-op
// monitor). publish receives each lag sample (e.g. metrics.SetReplicaLagSeconds).
func NewLagMonitor(replica *pgxpool.Pool, publish func(float64), interval time.Duration, log *slog.Logger) *LagMonitor {
	if interval <= 0 {
		interval = lagSampleInterval
	}
	return &LagMonitor{replica: replica, publish: publish, interval: interval, log: log}
}

// Start samples lag on a ticker until ctx is cancelled. It returns immediately
// (no goroutine) when no replica is configured, so the gauge stays 0. A query
// error logs a WARN and keeps the last published value — the monitor never
// crashes the process.
func (m *LagMonitor) Start(ctx context.Context) {
	if m == nil || m.replica == nil || m.publish == nil {
		return
	}
	go func() {
		t := time.NewTicker(m.interval)
		defer t.Stop()
		m.sample(ctx) // populate immediately, don't wait a full tick
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.sample(ctx)
			}
		}
	}()
}

func (m *LagMonitor) sample(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, lagSampleTimeout)
	defer cancel()
	var lag float64
	if err := m.replica.QueryRow(cctx, replicaLagSQL).Scan(&lag); err != nil {
		if m.log != nil {
			m.log.Warn("read-replica lag sample failed (gauge holds last value)", slog.String("err", err.Error()))
		}
		return
	}
	m.publish(lag)
}

// Check satisfies api.HealthChecker — (healthy, latencyMs, detail). A
// configured-but-unreachable replica is unhealthy; no replica configured is a
// healthy no-op (the feature is off, not broken), so it never trips /healthz.
func (m *LagMonitor) Check(ctx context.Context) (bool, int64, string) {
	if m == nil || m.replica == nil {
		return true, 0, "no replica configured"
	}
	cctx, cancel := context.WithTimeout(ctx, lagCheckTimeout)
	defer cancel()
	start := time.Now()
	var lag float64
	if err := m.replica.QueryRow(cctx, replicaLagSQL).Scan(&lag); err != nil {
		return false, time.Since(start).Milliseconds(), err.Error()
	}
	return true, time.Since(start).Milliseconds(), fmt.Sprintf("lag=%.1fs", lag)
}
