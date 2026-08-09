package forecast

// scope_failclosed_test.go — the forecasting half of the fail-closed scope rule.
//
// DailyBuckets and ProjectScope both take a scope that arrives, in production,
// straight off the ?scope= query parameter of
// GET /v1/workspaces/{wsID}/forecast — a string no caller validates. The
// scope→column mapping used to answer "" for BOTH "workspace, which needs no
// predicate" and "I have never heard of this scope", so an unrecognised value
// dropped the predicate: the daily buckets covered the WHOLE workspace and were
// returned as that scope's spend history, then CACHED under its key.
//
// The forecast for a scope nobody supports therefore looked like a busy one,
// and it was the only forecast a caller ever saw for that name.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/budgets"
)

// unknownScopeSrc is a source that would happily answer any scope. It exists so
// the refusal below is proven to come from the scope check and NOT from a
// downstream store failing — a refusal for the wrong reason is not a guard.
type unknownScopeSrc struct{ called bool }

func (s *unknownScopeSrc) PeriodSpent(context.Context, budgets.Budget) (float64, error) {
	s.called = true
	return 123.45, nil
}

func (s *unknownScopeSrc) DailyBuckets(context.Context, string, budgets.Scope, string, int, time.Time) ([]DayBucket, error) {
	s.called = true
	return []DayBucket{{Day: time.Now(), SpendUSD: 123.45}}, nil
}

func (s *unknownScopeSrc) Budgets(context.Context, string) ([]budgets.Budget, error) {
	s.called = true
	return nil, nil
}

// THE RED. Before the fix this returned a fully-populated forecast built from
// the whole workspace's spend, with a nil error.
func TestProjectScope_UnknownScope_Refused(t *testing.T) {
	src := &unknownScopeSrc{}
	f := New(src)
	for _, scope := range []budgets.Scope{"issue", "agent", "Team", ""} {
		fc, err := f.ProjectScope(context.Background(), "ws1", scope, "ENG-42", "monthly")
		if !errors.Is(err, budgets.ErrUnknownScope) {
			t.Errorf("ProjectScope(scope=%q) err = %v, want ErrUnknownScope (got forecast %+v)", scope, err, fc)
		}
	}
	// The refusal must happen BEFORE any spend is read. If the source was
	// touched, the scope check is sitting downstream of the very widening it
	// exists to prevent.
	if src.called {
		t.Error("an unknown scope reached the spend source — the refusal must come first, before any read is issued or cached")
	}
}

// NON-VACUITY: the supported scopes must still project. A check that refused
// everything would satisfy the test above and delete the feature.
func TestProjectScope_KnownScopes_StillProject(t *testing.T) {
	for _, scope := range []budgets.Scope{budgets.ScopeWorkspace, budgets.ScopeTeam, budgets.ScopeSprint} {
		src := &unknownScopeSrc{}
		f := New(src)
		if _, err := f.ProjectScope(context.Background(), "ws1", scope, "ENG", "monthly"); err != nil {
			t.Errorf("ProjectScope(scope=%q) = %v, want no error — supported scopes must still project", scope, err)
		}
		if !src.called {
			t.Errorf("ProjectScope(scope=%q) never read any spend — the projection went vacuous", scope)
		}
	}
}

// DailyBuckets is exported and is the OTHER place the scope→column mapping is
// consumed. ProjectScope refuses first in the shipped call path, so without
// this test the refusal inside DailyBuckets is unguarded — a positive control
// (PC6) found exactly that hole, which is why this test exists.
//
// A nil db is used deliberately: the refusal sits ABOVE the no-database short
// circuit, so it is reachable without Postgres. If it were moved back below,
// the unknown scopes here would return (nil, nil) and this test goes red.
func TestDailyBuckets_UnknownScope_Refused(t *testing.T) {
	s := &Store{}
	for _, scope := range []budgets.Scope{"issue", "agent", "Team", ""} {
		got, err := s.DailyBuckets(context.Background(), "ws1", scope, "ENG-42", 30, time.Now())
		if !errors.Is(err, budgets.ErrUnknownScope) {
			t.Errorf("DailyBuckets(scope=%q) = (%v, %v), want ErrUnknownScope — "+
				"an unfiltered bucket query returns the WHOLE workspace's daily spend as this scope's history", scope, got, err)
		}
	}
	// NON-VACUITY: a known scope must NOT be refused. With a nil db it returns
	// the empty no-database result, which is the pre-existing behaviour.
	for _, scope := range []budgets.Scope{budgets.ScopeWorkspace, budgets.ScopeTeam, budgets.ScopeSprint} {
		if _, err := s.DailyBuckets(context.Background(), "ws1", scope, "ENG", 30, time.Now()); err != nil {
			t.Errorf("DailyBuckets(scope=%q) = %v, want no error for a supported scope", scope, err)
		}
	}
}
