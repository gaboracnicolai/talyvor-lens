package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// errQueryPool is a pgxDB whose Query always errors — to prove a failed reload
// build never swaps the live map.
type errQueryPool struct{}

func (errQueryPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("boom: query failed")
}
func (errQueryPool) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("errQueryPool.QueryRow must not be called")
}
func (errQueryPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

// TestLoadAll_ErroredBuildLeavesOldMapIntact — build-then-swap must NOT publish a
// half-built (or empty) map when the DB read fails: the old cache stays live.
func TestLoadAll_ErroredBuildLeavesOldMapIntact(t *testing.T) {
	m := &Manager{
		pool: errQueryPool{},
		workspaces: map[string]*Workspace{
			"keep": {ID: "keep", LoggingPolicy: LoggingNone, CachePoolable: true},
		},
	}
	if err := m.LoadAll(context.Background()); err == nil {
		t.Fatal("expected LoadAll to error when Query fails")
	}
	// The OLD map must be intact — no swap on the error path.
	if _, ok := m.GetWorkspace("keep"); !ok {
		t.Fatal("errored LoadAll dropped the old map — 'keep' is gone (a swap happened on error)")
	}
	if got := m.GetLoggingPolicy("keep"); got != LoggingNone {
		t.Errorf("errored LoadAll altered logging policy: got %q, want none", got)
	}
	if !m.GetCachePoolable("keep") {
		t.Error("errored LoadAll altered cache_poolable: got false, want true")
	}
}

// TestGetters_DefaultOnMiss_Unchanged — the refactor must not change the
// default-on-unknown-workspace behavior of the privacy/policy getters.
func TestGetters_DefaultOnMiss_Unchanged(t *testing.T) {
	m := New(nil) // empty map, no pool
	if m.GetCachePoolable("unknown") {
		t.Error("GetCachePoolable(unknown) must default false")
	}
	if got := m.GetLoggingPolicy("unknown"); got != LoggingMetadata {
		t.Errorf("GetLoggingPolicy(unknown) = %q, must default LoggingMetadata", got)
	}
}
