package poolsafety_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/poolsafety"
)

// THE SEQUENCING TRAP THIS EXISTS FOR
//
// deploy/FULL-STACK-DEPLOY.md boots the gateway at step 2b and runs `lens poolcheck` at step
// 6b, because poolcheck needs a running container to exec into. A gateway that decides once at
// boot therefore finds no attestation, forces pooling off, and never reconsiders — so the
// FIRST deployment can never turn pooling on, silently. These tests pin the order.

const (
	liveModel     = "text-embedding-3-small"
	liveThreshold = 0.92
)

func attestedRow() stubRow {
	return stubRow{vals: []any{liveModel, liveThreshold, "review preamble · tiny diffs", 0.6534}}
}

// ⚠ THE DEPLOY ORDER, END TO END. This is the test that fails on a boot-only decision.
func TestGate_PoolcheckAfterBoot_TurnsPoolingOnWithoutARestart(t *testing.T) {
	ctx := context.Background()
	db := &stubDB{row: stubRow{err: poolsafety.ErrNoAttestation}}
	g := poolsafety.NewGate()

	// STEP 2b — the gateway boots against a database with no attestation yet.
	g.Refresh(ctx, db, liveModel, liveThreshold)
	if g.Attested() {
		t.Fatal("pooling was attested at boot with no attestation recorded — the gate must fail closed")
	}
	if g.Reason() == "" {
		t.Error("pooling is off with no reason; an operator cannot tell this apart from pooling " +
			"simply not being switched on")
	}

	// STEP 6b — poolcheck runs and records the attestation. NOTHING RESTARTS.
	db.row = attestedRow()
	changed := g.Refresh(ctx, db, liveModel, liveThreshold)

	if !g.Attested() {
		t.Fatal("poolcheck passed and recorded an attestation, but pooling is STILL off. " +
			"This is the runbook's own order (boot at 2b, poolcheck at 6b): a boot-only decision " +
			"means the first deployment can never turn pooling on, and the failure is silent.")
	}
	if !changed {
		t.Error("the decision flipped off→on but Refresh reported no change, so the transition " +
			"would never be logged")
	}
	if g.Reason() != "" {
		t.Errorf("an attested gate still carries a reason %q, which reads as a problem", g.Reason())
	}
}

// The reverse transition must also be live: an attestation that stops matching closes the gate
// without waiting for a restart.
func TestGate_AttestationStopsMatching_ClosesWithoutARestart(t *testing.T) {
	ctx := context.Background()
	db := &stubDB{row: attestedRow()}
	g := poolsafety.NewGate()
	g.Refresh(ctx, db, liveModel, liveThreshold)
	if !g.Attested() {
		t.Fatal("precondition: gate should be open")
	}

	// Someone re-records an attestation for a different embedder (e.g. poolcheck run against
	// another deployment's env pointed at this database).
	db.row = stubRow{vals: []any{"text-embedding-ada-002", liveThreshold, "p", 0.81}}
	if changed := g.Refresh(ctx, db, liveModel, liveThreshold); !changed {
		t.Error("the decision flipped on→off but Refresh reported no change")
	}
	if g.Attested() {
		t.Fatal("the attestation no longer describes this configuration, but pooling stayed on")
	}
	if !strings.Contains(g.Reason(), "ada-002") {
		t.Errorf("the reason does not name what changed: %q", g.Reason())
	}
}

// ⚠ A TRANSIENT DATABASE ERROR MUST NOT FLAP THE GATE. Re-reading on a timer means every
// connection blip is a chance to churn pooling off and on, which teaches operators that the
// signal is noise. After a conclusive read, an inconclusive one holds the last decision.
func TestGate_TransientReadFailure_HoldsTheLastDecision(t *testing.T) {
	ctx := context.Background()
	db := &stubDB{row: attestedRow()}
	g := poolsafety.NewGate()
	g.Refresh(ctx, db, liveModel, liveThreshold)

	db.row = stubRow{err: errors.New("connection reset by peer")}
	if changed := g.Refresh(ctx, db, liveModel, liveThreshold); changed {
		t.Error("a transient read failure reported a decision change")
	}
	if !g.Attested() {
		t.Fatal("a single failed read flipped pooling off. The previous decision was made from " +
			"real data; a connection blip is not evidence that the configuration became unsafe.")
	}
}

// ...but before ANY successful read there is no last decision to hold, so an unreadable
// attestation at boot must fail CLOSED rather than defaulting open.
func TestGate_ReadFailureAtBoot_FailsClosed(t *testing.T) {
	ctx := context.Background()
	db := &stubDB{row: stubRow{err: errors.New("connection refused")}}
	g := poolsafety.NewGate()
	g.Refresh(ctx, db, liveModel, liveThreshold)
	if g.Attested() {
		t.Fatal("an unreadable attestation at boot was treated as permission; \"could not confirm\" " +
			"and \"confirmed safe\" must not be the same answer")
	}
	if g.Reason() == "" {
		t.Error("failed closed with no reason recorded")
	}
}

// A gate nobody refreshed must be closed. The zero value is the safe value.
func TestGate_NeverRefreshed_IsClosed(t *testing.T) {
	g := poolsafety.NewGate()
	if g.Attested() {
		t.Fatal("an unrefreshed gate reported pooling as attested")
	}
	if g.Reason() == "" {
		t.Error("an unrefreshed gate gives no reason")
	}
}

// Repeated identical refreshes must not report a change, or every tick logs a transition.
func TestGate_SteadyState_ReportsNoChange(t *testing.T) {
	ctx := context.Background()
	db := &stubDB{row: attestedRow()}
	g := poolsafety.NewGate()
	g.Refresh(ctx, db, liveModel, liveThreshold)
	for i := 0; i < 3; i++ {
		if changed := g.Refresh(ctx, db, liveModel, liveThreshold); changed {
			t.Fatalf("refresh %d reported a change in steady state; a 30s ticker would flood the log", i)
		}
	}
}
