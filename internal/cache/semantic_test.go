package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

type stubEmbedder struct {
	vec []float32
	err error
}

func (s stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.vec, nil
}

func newTestSemanticCache(t *testing.T, embedder Embedder, threshold float64) (*SemanticCache, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return newSemanticCache(mock, embedder, threshold, time.Hour), mock
}

func TestSemanticCache_GetNoRowsReturnsNilNil(t *testing.T) {
	c, mock := newTestSemanticCache(t, stubEmbedder{vec: []float32{0.1, 0.2, 0.3}}, 0.9)

	mock.ExpectQuery(`SELECT id, response`).
		WithArgs(pgxmock.AnyArg(), "openai", "gpt-4", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "response", "similarity"}))

	got, err := c.Get(context.Background(), "openai", "gpt-4", "hello")
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil bytes on miss, got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSemanticCache_GetBelowThresholdReturnsNilNil(t *testing.T) {
	c, mock := newTestSemanticCache(t, stubEmbedder{vec: []float32{0.1, 0.2, 0.3}}, 0.9)

	mock.ExpectQuery(`SELECT id, response`).
		WithArgs(pgxmock.AnyArg(), "openai", "gpt-4", pgxmock.AnyArg()).
		WillReturnRows(
			pgxmock.NewRows([]string{"id", "response", "similarity"}).
				AddRow("11111111-1111-1111-1111-111111111111", "cached", 0.5),
		)

	got, err := c.Get(context.Background(), "openai", "gpt-4", "hello")
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil bytes below threshold, got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSemanticCache_GetAboveThresholdReturnsResponse(t *testing.T) {
	c, mock := newTestSemanticCache(t, stubEmbedder{vec: []float32{0.1, 0.2, 0.3}}, 0.9)

	const id = "11111111-1111-1111-1111-111111111111"
	mock.ExpectQuery(`SELECT id, response`).
		WithArgs(pgxmock.AnyArg(), "openai", "gpt-4", pgxmock.AnyArg()).
		WillReturnRows(
			pgxmock.NewRows([]string{"id", "response", "similarity"}).
				AddRow(id, "cached_payload", 0.95),
		)
	mock.ExpectExec(`UPDATE prompt_embeddings`).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	got, err := c.Get(context.Background(), "openai", "gpt-4", "hello")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "cached_payload" {
		t.Fatalf("got %q, want %q", got, "cached_payload")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSemanticCache_GetEmbedderErrorPropagates(t *testing.T) {
	embErr := errors.New("embed failed")
	c, mock := newTestSemanticCache(t, stubEmbedder{err: embErr}, 0.9)

	got, err := c.Get(context.Background(), "openai", "gpt-4", "hello")
	if !errors.Is(err, embErr) {
		t.Fatalf("expected embedder error to propagate, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil bytes on embedder error, got %q", got)
	}
	// no DB calls should have happened
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// cutoffMatcher asserts the DeleteStale cutoff is a time.Time inside the
// window (now-retention - slack, now-retention + slack) — i.e. that DeleteStale
// passes NOW()-retention rather than some unrelated timestamp.
type cutoffMatcher struct {
	retention time.Duration
	slack     time.Duration
}

func (m cutoffMatcher) Match(v interface{}) bool {
	got, ok := v.(time.Time)
	if !ok {
		return false
	}
	want := time.Now().UTC().Add(-m.retention)
	diff := got.Sub(want)
	if diff < 0 {
		diff = -diff
	}
	return diff <= m.slack
}

func TestSemanticCache_DeleteStaleDeletesAndReportsCount(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)

	const retention = 90 * 24 * time.Hour
	c := newSemanticCache(mock, stubEmbedder{}, 0.9, retention)

	mock.ExpectExec(`DELETE FROM prompt_embeddings`).
		WithArgs(cutoffMatcher{retention: retention, slack: time.Minute}).
		WillReturnResult(pgxmock.NewResult("DELETE", 7))

	n, err := c.DeleteStale(context.Background())
	if err != nil {
		t.Fatalf("DeleteStale: %v", err)
	}
	if n != 7 {
		t.Fatalf("expected 7 rows deleted, got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSemanticCache_DeleteStaleDisabledIsNoOp(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)

	// retention <= 0 disables sweeping: DeleteStale must not touch the DB.
	c := newSemanticCache(mock, stubEmbedder{}, 0.9, 0)

	n, err := c.DeleteStale(context.Background())
	if err != nil {
		t.Fatalf("DeleteStale: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows deleted when disabled, got %d", n)
	}
	// No expectations were registered; a stray query would fail this.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// zeroTimeMatcher asserts the arg is the zero time.Time — the sentinel
// freshnessCutoff returns when retention is disabled, making the serve-window
// filter (updated_at > $4) a no-op so rows of any age are servable.
type zeroTimeMatcher struct{}

func (zeroTimeMatcher) Match(v interface{}) bool {
	got, ok := v.(time.Time)
	return ok && got.IsZero()
}

// Option-B behavior: the serve window equals the retention window, so Get must
// gate the SELECT on updated_at > NOW()−retention (the 4th arg).
func TestSemanticCache_GetServeWindowUsesRetentionCutoff(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)

	const retention = 90 * 24 * time.Hour
	c := newSemanticCache(mock, stubEmbedder{vec: []float32{0.1, 0.2, 0.3}}, 0.9, retention)

	mock.ExpectQuery(`is_poolable = false`).
		WithArgs(pgxmock.AnyArg(), "openai", "gpt-4", cutoffMatcher{retention: retention, slack: time.Minute}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "response", "similarity"}))

	if _, err := c.Get(context.Background(), "openai", "gpt-4", "hello"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// retention <= 0 disables the window: the serve query gets the zero-time cutoff,
// so a row of any age is servable (kept-forever, pre-retention behavior).
func TestSemanticCache_GetServeWindowDisabledServesAllAges(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)

	c := newSemanticCache(mock, stubEmbedder{vec: []float32{0.1, 0.2, 0.3}}, 0.9, 0)

	mock.ExpectQuery(`is_poolable = false`).
		WithArgs(pgxmock.AnyArg(), "openai", "gpt-4", zeroTimeMatcher{}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "response", "similarity"}))

	if _, err := c.Get(context.Background(), "openai", "gpt-4", "hello"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSemanticCache_SetInsertsWithCorrectArgs(t *testing.T) {
	c, mock := newTestSemanticCache(t, stubEmbedder{}, 0.9)

	sum := sha256.Sum256([]byte("openai:gpt-4:hello"))
	wantHash := hex.EncodeToString(sum[:])

	mock.ExpectExec(`INSERT INTO prompt_embeddings`).
		WithArgs("openai", "gpt-4", wantHash, pgxmock.AnyArg(), "response_body").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	err := c.Set(
		context.Background(),
		"openai", "gpt-4", "hello",
		[]byte("response_body"),
		[]float32{0.1, 0.2, 0.3},
	)
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
