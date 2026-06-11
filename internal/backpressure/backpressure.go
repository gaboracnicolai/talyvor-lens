// Package backpressure bounds the concurrency of post-serve observational
// writes (attribution records, routing-pattern capture).
//
// WHY THIS EXISTS (empirical, 2026-06-11 load trial): these writers are
// "serve unaffected" by contract, but they had no concurrency bound. Under
// sustained overload (~250 req/s offered on a 2-core VM, 50 concurrent
// clients on one workspace) every request spawned/held an observational
// write; the writes timed out against the saturated pool; each timeout
// poisoned its pgx connection, which pgx destroyed and redialed. Live pool
// connections held at the LENS_DB_MAX_CONNS cap (100) the whole time — but
// the dying-connection churn piled to 1,126 not-yet-reaped TCP conns,
// PgBouncer's client count blew through max_client_conn (1000), and it
// began refusing ALL connections, starving the serve path itself
// (success rate 5–14% until load stopped). See issue #122.
//
// THE FIX SHAPE: drop-on-overflow, never queue. An observational write
// that can't get a slot is shed — losing an analytics row under overload
// is strictly better than churning the connection pool every serve-path
// query depends on. Queuing would only move the pile-up from the pool to
// memory; the correct backpressure for fire-and-forget telemetry is to
// stop firing.
package backpressure

import "sync/atomic"

// Limiter is a non-blocking concurrency bound. A nil *Limiter is valid and
// admits everything, so callers can hold one unconditionally and the
// operator can disable the bound with LENS_OBS_WRITE_MAX_CONCURRENT=0.
type Limiter struct {
	slots   chan struct{}
	dropped atomic.Int64
}

// New returns a Limiter that admits at most n concurrent holders.
// n <= 0 returns nil — the documented "bound off" switch.
func New(n int) *Limiter {
	if n <= 0 {
		return nil
	}
	return &Limiter{slots: make(chan struct{}, n)}
}

// TryAcquire reserves a slot without blocking. The caller must Release
// exactly once per true return. Nil-safe: a nil Limiter always admits.
func (l *Limiter) TryAcquire() bool {
	if l == nil {
		return true
	}
	select {
	case l.slots <- struct{}{}:
		return true
	default:
		l.dropped.Add(1)
		return false
	}
}

// Release frees a slot taken by TryAcquire. Nil-safe. An unmatched Release
// is a programming error but is deliberately non-blocking and non-panicking
// — observational plumbing must never take down the serve path.
func (l *Limiter) Release() {
	if l == nil {
		return
	}
	select {
	case <-l.slots:
	default:
	}
}

// Dropped returns the total number of writes shed so far. Exposed for
// metrics and for sampled drop logging.
func (l *Limiter) Dropped() int64 {
	if l == nil {
		return 0
	}
	return l.dropped.Load()
}

// LogDrop reports whether this drop should be logged: the first drop and
// then every 256th. Under a sustained overload drops arrive at request
// rate; logging each one would turn the incident into a WARN-log storm
// (observed: hundreds of identical lines in seconds during the trial).
func (l *Limiter) LogDrop() bool {
	if l == nil {
		return false
	}
	n := l.dropped.Load()
	return n == 1 || n%256 == 0
}
