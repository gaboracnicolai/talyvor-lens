package guardrails

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// errStore is a policyStore whose Query/Exec error on demand — to drive the
// DB-failure fail-direction without a database.
type errStore struct{ failQuery, failExec bool }

func (s errStore) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if s.failQuery {
		return nil, errors.New("db down")
	}
	return nil, errors.New("errStore.Query: unexpected call")
}
func (s errStore) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	if s.failExec {
		return pgconn.CommandTag{}, errors.New("db down")
	}
	return pgconn.CommandTag{}, nil
}

// TestNilStore_InMemoryOnly — a pool-less engine does zero DB I/O: SetPolicy /
// DeletePolicy / Load are map-only no-ops on the DB side, and it is never Degraded.
// This pins that tests stay DB-free.
func TestNilStore_InMemoryOnly(t *testing.T) {
	ctx := context.Background()
	e := newEngine() // no SetStore → store nil
	if err := e.SetPolicy(ctx, "ws", GuardrailPolicy{BlockedWords: []string{"x"}}); err != nil {
		t.Fatalf("nil-store SetPolicy must not error: %v", err)
	}
	if got := e.GetPolicy("ws"); len(got.BlockedWords) != 1 || got.BlockedWords[0] != "x" {
		t.Errorf("nil-store map write lost: %+v", got)
	}
	if err := e.Load(ctx); err != nil { // no-op
		t.Errorf("nil-store Load must be a no-op: %v", err)
	}
	if err := e.DeletePolicy(ctx, "ws"); err != nil {
		t.Errorf("nil-store DeletePolicy must not error: %v", err)
	}
	if e.Degraded() {
		t.Error("a nil-store engine must never be Degraded")
	}
}

// TestLoad_StartupDBError_DegradedAndDefaults — (3): a cold-start DB failure sets
// Degraded() and serves the LOCKED-DOWN default (PII redact + injection block ON).
func TestLoad_StartupDBError_DegradedAndDefaults(t *testing.T) {
	e := newEngine()
	e.store = errStore{failQuery: true}
	if err := e.Load(context.Background()); err == nil {
		t.Fatal("startup Load must error when the DB is down")
	}
	if !e.Degraded() {
		t.Error("startup load failure must set Degraded()")
	}
	p := e.GetPolicy("any-ws")
	if !p.EnablePII || p.PIIAction != ActionRedact || !p.EnableInjection || p.InjectionAction != ActionBlock {
		t.Errorf("degraded must still serve the locked-down default (PII redact + injection block); got %+v", p)
	}
}

// TestReload_DBError_RetainsLastGood_NotDegraded — THE adversarial core: a load
// failure must NEVER silently loosen a stricter-than-default workspace without
// Degraded firing. After a good load, a reload DB error keeps the stricter policy
// (PIIAction=Block) AND stays non-degraded (because we're serving real policies,
// not defaults — the operator is not falsely alarmed, and the tightening holds).
func TestReload_DBError_RetainsLastGood_NotDegraded(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	// simulate a prior successful load carrying a STRICTER-than-default policy.
	e.mu.Lock()
	e.policies = map[string]*GuardrailPolicy{
		"ws": {WorkspaceID: "ws", EnablePII: true, PIIAction: ActionBlock, BlockedWords: []string{"keep"}},
	}
	e.mu.Unlock()
	e.loaded.Store(true)

	e.store = errStore{failQuery: true}
	if err := e.Reload(ctx); err == nil {
		t.Fatal("Reload must error when the DB is down")
	}
	if e.Degraded() {
		t.Error("a reload failure over a good map must NOT be Degraded (we serve last-good real policies)")
	}
	got := e.GetPolicy("ws")
	if got.PIIAction != ActionBlock || len(got.BlockedWords) != 1 || got.BlockedWords[0] != "keep" {
		t.Errorf("reload failure must RETAIN the stricter last-good policy, never downgrade to default: %+v", got)
	}
}
