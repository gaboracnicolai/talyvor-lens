package routingbrain

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const defaultRefreshInterval = 5 * time.Minute

// Config tunes the serving-side Brain. Zero fields fall back to defaults.
type Config struct {
	Enabled         bool
	RefreshInterval time.Duration
}

// recStore is the read seam the Brain refreshes from (*Store satisfies it). Read
// only — the serving Brain can never write, let alone mint.
type recStore interface {
	LoadRecommendations(ctx context.Context) ([]Recommendation, error)
	LoadAutonomous(ctx context.Context) ([]string, error)
}

// Brain is the SERVING-side reader: an in-memory cache of the offline brain's
// recommendations + the autonomous opt-in set, refreshed on a timer. Every serve-
// path lookup is a pure in-memory read (NEVER a DB query on the request path),
// mirroring the routing Advisor's discipline. cost is the blended per-model price
// the hard floor compares on.
type Brain struct {
	src  recStore
	cost func(model string) float64
	cfg  Config
	now  func() time.Time

	mu          sync.RWMutex
	recs        map[string]map[int]Recommendation // wsID → difficulty → rec
	autonomous  map[string]bool
	lastRefresh time.Time
}

// New builds the serving Brain. cost prices models for the hard floor (nil ⇒
// everything free ⇒ the cost floor never binds).
func New(src recStore, cost func(string) float64, cfg Config) *Brain {
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = defaultRefreshInterval
	}
	if cost == nil {
		cost = func(string) float64 { return 0 }
	}
	return &Brain{src: src, cost: cost, cfg: cfg, now: time.Now, recs: map[string]map[int]Recommendation{}, autonomous: map[string]bool{}}
}

// Enabled reports whether the brain is on. When off, the serving path reads no
// recommendation (routing is byte-for-byte unchanged) and the offline job doesn't run.
func (b *Brain) Enabled() bool { return b != nil && b.cfg.Enabled }

// Refresh reloads the recommendation cache + autonomous set from the store. The
// only DB touch in the serving Brain.
func (b *Brain) Refresh(ctx context.Context) error {
	if b == nil || b.src == nil {
		return nil
	}
	recs, err := b.src.LoadRecommendations(ctx)
	if err != nil {
		return err
	}
	auto, err := b.src.LoadAutonomous(ctx)
	if err != nil {
		return err
	}
	next := make(map[string]map[int]Recommendation)
	for _, r := range recs {
		if next[r.WorkspaceID] == nil {
			next[r.WorkspaceID] = map[int]Recommendation{}
		}
		rr := r
		next[r.WorkspaceID][r.Difficulty] = rr
	}
	nextAuto := make(map[string]bool, len(auto))
	for _, ws := range auto {
		nextAuto[ws] = true
	}
	b.mu.Lock()
	b.recs = next
	b.autonomous = nextAuto
	b.lastRefresh = b.now()
	b.mu.Unlock()
	return nil
}

// StartRefresh does an immediate refresh then refreshes on the configured interval
// until ctx is cancelled. Only runs when enabled (off ⇒ cache stays empty ⇒ every
// lookup misses).
func (b *Brain) StartRefresh(ctx context.Context) {
	if b == nil || !b.cfg.Enabled || b.src == nil {
		return
	}
	if err := b.Refresh(ctx); err != nil {
		slog.Warn("routingbrain: initial refresh failed", slog.String("err", err.Error()))
	}
	go func() {
		t := time.NewTicker(b.cfg.RefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := b.Refresh(ctx); err != nil {
					slog.Warn("routingbrain: refresh failed", slog.String("err", err.Error()))
				}
			}
		}
	}()
}

// Lookup returns the precomputed recommendation for a (workspace, difficulty), if any.
// Pure in-memory.
func (b *Brain) Lookup(workspaceID string, difficulty int) (*Recommendation, bool) {
	if b == nil {
		return nil, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if byDiff, ok := b.recs[workspaceID]; ok {
		if r, ok := byDiff[difficulty]; ok {
			rr := r
			return &rr, true
		}
	}
	return nil, false
}

// ModeFor returns a workspace's posture: autonomous only if it opted in, else advisory.
func (b *Brain) ModeFor(workspaceID string) Mode {
	if b == nil {
		return ModeAdvisory
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.autonomous[workspaceID] {
		return ModeAutonomous
	}
	return ModeAdvisory
}

// Resolve is the SERVE-TIME entry: look up the recommendation for the request's
// (workspace, difficulty) and apply the advisory/autonomous decision under the hard
// floor. Returns ok=false when the brain is off or there is no recommendation to
// surface (the caller then leaves routing untouched). safeModel is the model the
// existing router would use; allowedModels is the workspace's live allow-list.
func (b *Brain) Resolve(workspaceID string, difficulty int, safeModel string, allowedModels []string) (Decision, bool) {
	if !b.Enabled() {
		return Decision{}, false
	}
	rec, ok := b.Lookup(workspaceID, difficulty)
	if !ok {
		return Decision{}, false
	}
	safe := SafeDecision{Model: safeModel, Cost: b.cost(safeModel)}
	recCost := b.cost(rec.Model)
	return Decide(b.ModeFor(workspaceID), rec, safe, recCost, allowedModels), true
}
