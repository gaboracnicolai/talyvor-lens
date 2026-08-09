package budgets

// scope_failclosed_realpg_test.go — THE LANDMINE THIS FILE EXISTS TO DEFUSE.
//
// A budget's realized spend is summed by mapping its scope to a token_events
// COLUMN (ScopeColumn). The workspace scope deliberately has NO column — its
// predicate is workspace_id itself. Every other scope must add a column
// predicate. Until now "I do not know this scope" and "this scope needs no
// predicate" were THE SAME ANSWER: the empty string.
//
// So a scope the mapping does not know did not fail. ReconcileSpent dropped the
// predicate and summed the ENTIRE WORKSPACE, returning that as the scope's own
// spend — MEASURED at 7.00 for a workspace holding 7.00 whose named scope had
// spent 1.00, with a nil error. On a hard_block budget that is not an
// over-count, it is a workspace-wide outage: the budget sits permanently over
// its limit and rejects every request in the workspace, while the API and the
// dashboard show an ordinary budget row.
//
// This is not hypothetical. W3.3 asks for sub-budgets per ISSUE, and the two
// obvious places to add one are validScope() and migration 0028's CHECK
// constraint — NEITHER of which is this mapping. The next session to do the
// obvious thing lands exactly here.
//
// THE INVARIANT: an unknown scope is REFUSED, never silently widened to the
// whole workspace. costanomaly's UnitCostsWindow already fails closed on an
// unrecognised unit kind (a closed whitelist + an explicit error); this brings
// the two spend readers that did not — budgets.ReconcileSpent and
// forecast.DailyBuckets — into line with the one that did.
//
// Real Postgres, against the real SQL: the defect is in what the query SUMS,
// which a mock cannot show. Schema follows this package's existing idiom
// (updatespent_workspace_integration_test.go): a private schema holding a
// faithful subset of the production table, so the test needs no extensions.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const scopeFCSchema = "lens_budgets_scope_failclosed"

// scopeFCStore builds a private schema holding a faithful subset of
// token_events — every column ReconcileSpent reads or filters on
// (workspace_id, team, sprint_id, cost_usd, created_at). The column NAMES are
// the ones ScopeColumn returns; TestScopeColumn_KnownVsUnknown pins that
// mapping, so a rename that broke the real query fails there rather than
// passing vacuously here.
func scopeFCStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG scope fail-closed test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = scopeFCSchema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS ` + scopeFCSchema + ` CASCADE`,
		`CREATE SCHEMA ` + scopeFCSchema,
		`CREATE TABLE token_events (
			id           BIGSERIAL PRIMARY KEY,
			workspace_id TEXT NOT NULL DEFAULT 'default',
			team         TEXT NOT NULL DEFAULT '',
			sprint_id    TEXT NOT NULL DEFAULT '',
			cost_usd     DOUBLE PRECISION NOT NULL DEFAULT 0,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	return NewStore(pool), pool
}

// seedScopeFCSpend lays down a workspace whose TOTAL differs from every single
// scope's share, so "returned the workspace total" and "returned this scope's
// own spend" can never be mistaken for one another.
//
// workspace total 7.00 = team ENG 1.00 + team SALES 2.00 + unattributed 4.00.
// Sprint S1 holds 3.00 — a third distinct figure, so a sprint result cannot be
// confused with either a team result or the workspace total.
func seedScopeFCSpend(t *testing.T, pool *pgxpool.Pool, ws string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO token_events
		(workspace_id, team, sprint_id, cost_usd)
		VALUES ($1,'ENG','S1',1.00), ($1,'SALES','S1',2.00), ($1,'','',4.00)`, ws); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

const scopeFCWorkspaceTotal = 7.00

// THE RED. An unknown scope must be refused. Before the fix each of these
// returned 7.00 — the workspace total — with a nil error.
func TestReconcileSpent_UnknownScope_RefusedNotWidened(t *testing.T) {
	st, pool := scopeFCStore(t)
	const ws = "ws-scopefc"
	seedScopeFCSpend(t, pool, ws)
	ctx := context.Background()

	// "issue" is the scope W3.3 will reach for. The others stand in for the
	// general case: a case-wrong scope, a typo, and the zero value an un-set
	// struct field carries.
	for _, scope := range []Scope{Scope("issue"), Scope("Team"), Scope("sprnit"), Scope("")} {
		got, err := st.ReconcileSpent(ctx, Budget{WorkspaceID: ws, Scope: scope, ScopeID: "ENG", Period: "monthly"})
		if !errors.Is(err, ErrUnknownScope) {
			t.Errorf("ReconcileSpent(scope=%q) = (%v, %v); want ErrUnknownScope. "+
				"Dropping the predicate sums the WHOLE WORKSPACE (%v here) and reports it as this scope's spend — "+
				"a hard_block budget on it then rejects every request in the workspace.",
				scope, got, err, scopeFCWorkspaceTotal)
		}
		if got != 0 {
			t.Errorf("ReconcileSpent(scope=%q) returned spend %v alongside its refusal; a refused read must carry no figure", scope, got)
		}
	}
}

// The refusal must sit ABOVE the no-database short circuit. A Store with no
// pool answers ReconcileSpent from the persisted snapshot, so a refusal placed
// below that early return is unreachable whenever a pool is absent — and every
// other test here supplies a real pool, which is how a positive control (PC8)
// caught the ordering being unguarded. Needs no Postgres, by design.
func TestReconcileSpent_UnknownScope_RefusedEvenWithoutAPool(t *testing.T) {
	st := NewStore(nil)
	got, err := st.ReconcileSpent(context.Background(), Budget{
		WorkspaceID: "ws1", Scope: Scope("issue"), ScopeID: "ENG-42", Period: "monthly", SpentUSD: 42,
	})
	if !errors.Is(err, ErrUnknownScope) {
		t.Errorf("ReconcileSpent(no pool, scope=issue) = (%v, %v), want ErrUnknownScope — "+
			"a refusal below the no-database short circuit is no refusal at all", got, err)
	}
	// NON-VACUITY: a KNOWN scope must still get the no-database answer it
	// always got (the persisted snapshot), not a refusal.
	if got, err := st.ReconcileSpent(context.Background(), Budget{
		WorkspaceID: "ws1", Scope: ScopeTeam, ScopeID: "ENG", Period: "monthly", SpentUSD: 42,
	}); err != nil || got != 42 {
		t.Errorf("ReconcileSpent(no pool, scope=team) = (%v, %v), want (42, nil) — the no-database path is unchanged for supported scopes", got, err)
	}
}

// THE NON-VACUITY HALF. A "fix" that refused everything would satisfy the test
// above and destroy the product, so the known scopes are pinned to their EXACT
// figures — including the workspace scope, whose whole-workspace sum is correct
// BY DESIGN and must not be swept up by a fail-closed rule.
func TestReconcileSpent_KnownScopes_Unchanged(t *testing.T) {
	st, pool := scopeFCStore(t)
	const ws = "ws-scopefc"
	seedScopeFCSpend(t, pool, ws)
	ctx := context.Background()

	cases := []struct {
		scope   Scope
		scopeID string
		want    float64
	}{
		{ScopeWorkspace, ws, scopeFCWorkspaceTotal}, // no column predicate BY DESIGN
		{ScopeTeam, "ENG", 1.00},
		{ScopeTeam, "SALES", 2.00},
		{ScopeTeam, "NOSUCH", 0.00}, // a KNOWN scope with no rows is 0, not the workspace total
		{ScopeSprint, "S1", 3.00},
		{ScopeSprint, "NOSUCH", 0.00},
	}
	for _, c := range cases {
		got, err := st.ReconcileSpent(ctx, Budget{WorkspaceID: ws, Scope: c.scope, ScopeID: c.scopeID, Period: "monthly"})
		if err != nil {
			t.Errorf("ReconcileSpent(%s/%s): unexpected error %v", c.scope, c.scopeID, err)
			continue
		}
		if got != c.want {
			t.Errorf("ReconcileSpent(%s/%s) = %v, want %v", c.scope, c.scopeID, got, c.want)
		}
	}
}

// ScopeColumn must distinguish "known, and deliberately has no column"
// (workspace) from "unknown" — the two cases that used to be one answer.
func TestScopeColumn_KnownVsUnknown(t *testing.T) {
	cases := []struct {
		scope   Scope
		wantCol string
		wantOK  bool
	}{
		{ScopeWorkspace, "", true}, // known; workspace_id IS the predicate
		{ScopeTeam, "team", true},
		{ScopeSprint, "sprint_id", true},
		{Scope("issue"), "", false},
		{Scope(""), "", false},
		{Scope("workspace_id"), "", false},
	}
	for _, c := range cases {
		col, ok := ScopeColumn(c.scope)
		if col != c.wantCol || ok != c.wantOK {
			t.Errorf("ScopeColumn(%q) = (%q, %v), want (%q, %v)", c.scope, col, ok, c.wantCol, c.wantOK)
		}
	}
}

// The mapping and the validator must agree in BOTH directions, enumerated from
// ONE list. A scope the validator accepts but the mapping cannot filter is the
// outage above. A scope the mapping knows but the validator rejects is a dead
// branch that reads as support which does not exist. Adding a scope in one
// place and not the other is a red here, not a surprise in production.
func TestScopeMapping_AgreesWithValidator(t *testing.T) {
	supported := []Scope{ScopeWorkspace, ScopeTeam, ScopeSprint}
	for _, s := range supported {
		if !validScope(s) {
			t.Errorf("scope %q is listed as supported but validScope rejects it", s)
		}
		if _, ok := ScopeColumn(s); !ok {
			t.Errorf("scope %q passes validScope but ScopeColumn cannot filter it — a budget on it would sum the WHOLE WORKSPACE", s)
		}
	}
	// Nothing outside the list may pass the validator. "issue" and "agent" are
	// named explicitly because both are claimed on the public Lens page and
	// NEITHER is a budget scope today (per-agent is a separate LXC ceiling keyed
	// on the scoped API key — agent_lxc_subbudgets, migration 0079 — not a row
	// in this table).
	for _, s := range []Scope{"issue", "agent", "user", "project", ""} {
		if validScope(s) {
			t.Errorf("validScope accepts %q, which is not in the supported list — classify it (and teach ScopeColumn) or reject it", s)
		}
	}
}

// The period window is untouched by this change; pinned so the fix is provably
// confined to the scope mapping.
func TestPeriodBounds_Untouched(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	s, e, ok := PeriodBounds("monthly", now)
	if !ok || s.Day() != 1 || e.Month() != time.September {
		t.Errorf("monthly bounds moved: start=%v end=%v ok=%v", s, e, ok)
	}
	if _, _, ok := PeriodBounds("total", now); ok {
		t.Error("total must remain open-ended")
	}
}
