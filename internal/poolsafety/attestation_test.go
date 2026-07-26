package poolsafety_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/poolsafety"
)

// WHY THIS IS TESTED IN CI RATHER THAN BY HAND
//
// The boot binding is the control that makes the preflight matter: without it, `lens
// poolcheck` only protects deploys where somebody remembered to run it, and an operator
// editing .env on the host at 11pm is exactly the case it exists for. A control verified
// only by a manual run is a control that stops working the first time nobody runs it.
//
// The decision table below is the whole behaviour. Each row is a configuration change an
// operator can make without thinking about pooling at all.

func TestMatchesLive_DecisionTable(t *testing.T) {
	attested := poolsafety.Attestation{
		EmbeddingModel: "text-embedding-3-small",
		Threshold:      0.92,
		WorstPair:      "review preamble · tiny diffs",
		WorstScore:     0.6534,
	}

	cases := []struct {
		name      string
		model     string
		threshold float64
		wantOK    bool
		why       string
	}{
		{
			name: "unchanged configuration", model: "text-embedding-3-small", threshold: 0.92,
			wantOK: true,
			why:    "the running configuration IS the measured one",
		},
		{
			name: "embedding model swapped for a cheaper one", model: "text-embedding-ada-002", threshold: 0.92,
			wantOK: false,
			why:    "a different model embeds into a different space; the measurement does not transfer",
		},
		{
			name: "threshold lowered", model: "text-embedding-3-small", threshold: 0.80,
			wantOK: false,
			why:    "matching is now wider than anything poolcheck examined",
		},
		{
			// The conservative direction MUST NOT trip the guard. A control that cries wolf
			// on a safety improvement is one operators learn to route around.
			name: "threshold raised", model: "text-embedding-3-small", threshold: 0.95,
			wantOK: true,
			why:    "raising the threshold makes matching strictly harder than what passed",
		},
		{
			// Guards against a lazy "threshold > WorstScore" rule: 0.70 clears the measured
			// worst pair (0.6534) but is far below the margin that was actually attested.
			name:  "threshold lowered but still above the worst measured pair",
			model: "text-embedding-3-small", threshold: 0.70,
			wantOK: false,
			why:    "the corpus bounds the corpus, not live traffic — clearing the sample is not proof",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := attested.MatchesLive(tc.model, tc.threshold)
			if ok != tc.wantOK {
				t.Fatalf("MatchesLive(%q, %v) = %v, want %v — %s\n  reason given: %q",
					tc.model, tc.threshold, ok, tc.wantOK, tc.why, reason)
			}
			if !ok && reason == "" {
				t.Error("pooling was forced off with an EMPTY reason; a silent forced-off state is " +
					"indistinguishable from pooling simply not being configured")
			}
			if ok && reason != "" {
				t.Errorf("a matching configuration produced a reason string %q, which will read as a problem", reason)
			}
		})
	}
}

// stubRow / stubDB stand in for pgx without importing it, mirroring the split interfaces.
type stubRow struct {
	err  error
	vals []any
}

func (r stubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = r.vals[i].(string)
		case *float64:
			*d = r.vals[i].(float64)
		}
	}
	return nil
}

type stubDB struct {
	row      stubRow
	execErr  error
	execArgs []any
}

func (s *stubDB) QueryRow(context.Context, string, ...any) poolsafety.Row { return s.row }
func (s *stubDB) Exec(_ context.Context, _ string, args ...any) error {
	s.execArgs = args
	return s.execErr
}

// A read failure must NOT be reported as a valid attestation. This is the fail-closed
// property: an unreadable attestation is treated as absent, never as permission.
func TestLoad_ErrorIsNotAnAttestation(t *testing.T) {
	db := &stubDB{row: stubRow{err: errors.New("connection reset")}}
	got, err := poolsafety.Load(context.Background(), db)
	if err == nil {
		t.Fatal("a failed read returned no error; the caller would treat the zero Attestation as real " +
			"and compare a live config against an empty model name")
	}
	if got != (poolsafety.Attestation{}) {
		t.Errorf("a failed read returned a partially populated attestation %+v", got)
	}
}

// Record must persist the measurement, not just the verdict: WorstPair/WorstScore are what
// let a later reader see how much margin the passing run actually had.
func TestRecord_PersistsTheMeasurementNotJustTheVerdict(t *testing.T) {
	db := &stubDB{}
	a := poolsafety.Attestation{
		EmbeddingModel: "text-embedding-3-small", Threshold: 0.92,
		WorstPair: "review preamble · tiny diffs", WorstScore: 0.6534,
	}
	if err := poolsafety.Record(context.Background(), db, a); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(db.execArgs) != 4 {
		t.Fatalf("expected model, threshold, worst pair and worst score to be stored; got %d args: %v",
			len(db.execArgs), db.execArgs)
	}
	if db.execArgs[0] != a.EmbeddingModel || db.execArgs[1] != a.Threshold {
		t.Errorf("attestation did not record the configuration it attests to: %v", db.execArgs)
	}
	if db.execArgs[3] != a.WorstScore {
		t.Errorf("worst score not recorded (%v); without it the stored row cannot show how much "+
			"margin the passing run had", db.execArgs[3])
	}
}

// A write failure must surface. `lens poolcheck` reporting success while failing to record
// would leave the gateway forcing pooling off with no indication why.
func TestRecord_WriteFailureSurfaces(t *testing.T) {
	db := &stubDB{execErr: errors.New("read-only transaction")}
	if err := poolsafety.Record(context.Background(), db, poolsafety.Attestation{}); err == nil {
		t.Fatal("a failed attestation write was swallowed")
	}
}
