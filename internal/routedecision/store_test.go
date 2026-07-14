package routedecision

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---- fakes (no PG) ----

type fakeRow struct {
	total, override, actual, cf int64
	err                         error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	vals := []int64{r.total, r.override, r.actual, r.cf}
	for i, d := range dest {
		if p, ok := d.(*int64); ok && i < len(vals) {
			*p = vals[i]
		}
	}
	return nil
}

type fakeReadDB struct{ row fakeRow }

func (d fakeReadDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row { return d.row }

// Summarize computes the override RATE and the estimated delta from the aggregate columns.
func TestSummarize_OverrideRateAndDelta(t *testing.T) {
	r := NewReader(fakeReadDB{row: fakeRow{total: 4, override: 1, actual: 300, cf: 500}})
	s, err := r.Summarize(context.Background(), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalRequests != 4 || s.OverrideCount != 1 {
		t.Fatalf("total/override = %d/%d, want 4/1", s.TotalRequests, s.OverrideCount)
	}
	if s.OverrideRate != 0.25 {
		t.Errorf("override rate = %v, want 0.25", s.OverrideRate)
	}
	if s.EstimatedCostDeltaU != 200 {
		t.Errorf("estimated delta = %d, want 200 (cf 500 − actual 300)", s.EstimatedCostDeltaU)
	}
}

// Zero requests must not divide by zero — rate is 0.
func TestSummarize_ZeroRequests_NoDivideByZero(t *testing.T) {
	r := NewReader(fakeReadDB{row: fakeRow{total: 0}})
	s, err := r.Summarize(context.Background(), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.OverrideRate != 0 {
		t.Errorf("override rate = %v, want 0 for zero requests", s.OverrideRate)
	}
}

// ---- Record: costs are integer µ-units (no float in the persisted amount) ----

type captureWriteDB struct{ args []any }

func (d *captureWriteDB) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	d.args = args
	return pgconn.CommandTag{}, nil
}

func TestRecord_CostsAreIntegerMicroUnits(t *testing.T) {
	db := &captureWriteDB{}
	w := &Writer{db: db}
	if err := w.Record(context.Background(), RouteDecision{
		WorkspaceID: "ws1", BaselineModel: "big", ActualModel: "small", CohortOverrode: true,
		InputTokens: 100, OutputTokens: 50, ActualCostU: 300, CounterfactualCostEstimateU: 500,
	}); err != nil {
		t.Fatal(err)
	}
	// args 9 and 10 are actual_cost_u and counterfactual_cost_estimate_u — both int64, never float.
	if _, ok := db.args[8].(int64); !ok {
		t.Errorf("actual_cost_u arg is %T, want int64 (SEC-2: no float in a stored amount)", db.args[8])
	}
	if _, ok := db.args[9].(int64); !ok {
		t.Errorf("counterfactual_cost_estimate_u arg is %T, want int64", db.args[9])
	}
}
