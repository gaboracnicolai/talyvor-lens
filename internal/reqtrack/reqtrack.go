// Package reqtrack is what lets /healthz tell a hang from a restart from a network blip (B27.11).
//
// It keeps the requests Lens is serving right now, the slowest request of the last five minutes, and
// the phase each request is in — waiting for a database connection, running a query, in Redis,
// waiting on the upstream provider, or in Lens's own code — and logs every request that runs past the
// slow threshold, once when it crosses it (so a request that never finishes is still seen) and again
// when it ends. Requests are named by method and route TEMPLATE only, never the raw path, so nothing
// this package reports or logs carries an id.
package reqtrack

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
)

// Phase is where a request is spending its time right now.
type Phase uint32

const (
	Handler   Phase = iota // Lens's own code — the default
	DBAcquire              // waiting for a connection from the database pool
	DBQuery                // a query is running
	Redis                  // a Redis command is running
	Upstream               // waiting on the provider: the round trip, or a read of its response body
)

var phaseNames = [...]string{"handler", "db_acquire", "db_query", "redis", "upstream"}

func (p Phase) String() string {
	if int(p) < len(phaseNames) {
		return phaseNames[p]
	}
	return "unknown"
}

// Request is one request being tracked.
type Request struct {
	route     string
	start     time.Time
	phase     atomic.Uint32
	slowPhase atomic.Uint32 // phase+1 when it crossed the slow threshold; 0 = has not
	longLived atomic.Bool
	done      atomic.Bool
}

// Phase is the phase the request is in now.
func (q *Request) Phase() Phase { return Phase(q.phase.Load()) }

type ctxKey struct{}

// FromContext returns the request ctx belongs to, or nil for work that is not serving a request.
func FromContext(ctx context.Context) *Request {
	q, _ := ctx.Value(ctxKey{}).(*Request)
	return q
}

// Mark is a phase entered by Enter; Exit puts back the phase the request was in before.
type Mark struct {
	q    *Request
	prev Phase
}

// Enter marks the request ctx belongs to as being in phase p until Exit. Outside a request it does nothing.
func Enter(ctx context.Context, p Phase) Mark {
	q := FromContext(ctx)
	if q == nil {
		return Mark{}
	}
	return Mark{q: q, prev: Phase(q.phase.Swap(uint32(p)))}
}

// Exit ends the phase Enter began.
func (m Mark) Exit() {
	if m.q != nil {
		m.q.phase.Store(uint32(m.prev))
	}
}

// MarkLongLived tells the tracker the request is a subscription that is meant to stay open (an SSE
// stream), so it is counted as in flight but is neither "slowest" nor logged as slow.
func MarkLongLived(ctx context.Context) {
	if q := FromContext(ctx); q != nil {
		q.longLived.Store(true)
	}
}

const (
	slotSeconds = 10
	windowSlots = 30 // 30 × 10s = the last five minutes, to ten-second resolution
)

// slot is the slowest request that finished in one ten-second slice.
type slot struct {
	idx   int64 // unix seconds / slotSeconds
	route string
	ms    int64
	phase string
}

// Tracker is the in-flight set and the five-minute window. One per process.
type Tracker struct {
	slowAfter time.Duration
	log       *slog.Logger

	mu       sync.Mutex
	inflight map[*Request]struct{}
	window   [windowSlots]slot
}

// New returns a tracker that logs requests slower than slowAfter to log.
func New(slowAfter time.Duration, log *slog.Logger) *Tracker {
	return &Tracker{slowAfter: slowAfter, log: log, inflight: map[*Request]struct{}{}}
}

// Middleware tracks every request routes serves. routes is the router the middleware is installed on;
// it is asked for the route template up front so the template is known while the request still runs.
func (t *Tracker) Middleware(routes chi.Routes) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.RawPath
			if path == "" {
				path = r.URL.Path
			}
			route := routes.Find(chi.NewRouteContext(), r.Method, path)
			if route == "" {
				route = "other"
			}
			q := &Request{route: r.Method + " " + route, start: time.Now()}
			t.mu.Lock()
			t.inflight[q] = struct{}{}
			t.mu.Unlock()
			timer := time.AfterFunc(t.slowAfter, func() { t.crossed(q) })
			defer func() {
				timer.Stop()
				t.finish(q)
			}()
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, q)))
		})
	}
}

// crossed runs when a request is still going at the slow threshold.
func (t *Tracker) crossed(q *Request) {
	if q.longLived.Load() || q.done.Load() {
		return
	}
	p := q.Phase()
	q.slowPhase.Store(uint32(p) + 1)
	t.log.Warn("slow request still running",
		slog.String("route", q.route), slog.String("phase", p.String()),
		slog.Int64("elapsed_ms", time.Since(q.start).Milliseconds()))
}

func (t *Tracker) finish(q *Request) {
	q.done.Store(true)
	now := time.Now()
	d := now.Sub(q.start)
	slow := d >= t.slowAfter && !q.longLived.Load()
	phase := ""
	if slow {
		phase = q.Phase().String()
		if sp := q.slowPhase.Load(); sp != 0 {
			phase = Phase(sp - 1).String()
		}
	}

	t.mu.Lock()
	delete(t.inflight, q)
	if !q.longLived.Load() {
		idx := now.Unix() / slotSeconds
		s := &t.window[idx%windowSlots]
		if s.idx != idx {
			*s = slot{idx: idx}
		}
		if s.route == "" || d.Milliseconds() > s.ms {
			*s = slot{idx: idx, route: q.route, ms: d.Milliseconds(), phase: phase}
		}
	}
	t.mu.Unlock()

	if slow {
		t.log.Warn("slow request",
			slog.String("route", q.route), slog.String("phase", phase), slog.Int64("ms", d.Milliseconds()))
	}
}

// Slowest is the slowest request of the window. Phase is the phase it was in when it crossed the slow
// threshold, or for a request still running the phase it is in now; empty for a finished request that
// was never slow.
type Slowest struct {
	Route   string `json:"route"`
	Ms      int64  `json:"ms"`
	Phase   string `json:"phase,omitempty"`
	Running bool   `json:"running"`
}

// Snapshot is the "requests" section of /healthz.
type Snapshot struct {
	InFlight  int      `json:"in_flight"`
	LongLived int      `json:"long_lived"`
	Slowest5m *Slowest `json:"slowest_5m"`
}

// Snapshot reports the requests in flight and the slowest of the last five minutes, finished or still
// running. The request ctx belongs to — the /healthz probe asking — is left out.
func (t *Tracker) Snapshot(ctx context.Context) Snapshot {
	self := FromContext(ctx)
	now := time.Now()
	var s Snapshot
	t.mu.Lock()
	defer t.mu.Unlock()
	for q := range t.inflight {
		if q == self {
			continue
		}
		s.InFlight++
		if q.longLived.Load() {
			s.LongLived++
			continue
		}
		if ms := now.Sub(q.start).Milliseconds(); s.Slowest5m == nil || ms > s.Slowest5m.Ms {
			s.Slowest5m = &Slowest{Route: q.route, Ms: ms, Phase: q.Phase().String(), Running: true}
		}
	}
	cur := now.Unix() / slotSeconds
	for _, sl := range t.window {
		if sl.route == "" || sl.idx <= cur-windowSlots {
			continue
		}
		if s.Slowest5m == nil || sl.ms > s.Slowest5m.Ms {
			s.Slowest5m = &Slowest{Route: sl.route, Ms: sl.ms, Phase: sl.phase}
		}
	}
	return s
}
