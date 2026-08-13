package compressmeasure

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// AGAINST REAL POSTGRES, because every claim here is about what a query returns:
// that the tenant boundary is in the WHERE clause, that the window excludes old
// rows, that a retried write cannot inflate the denominator, and that an empty
// table answers "0 requests" rather than an error.
//
// ⚠ THE SKIP IS THE HAZARD. With LENS_TEST_DATABASE_URL unset these tests skip,
// and a skip reads exactly like a pass in a local run. CI sets it; a local green
// without it has proved nothing about any of the above.

func measurePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_compmeasure_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = ac.Close(ctx)
		t.Fatalf("create: %v", err)
	}
	_ = ac.Close(ctx)
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	mc, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		_ = mc.Close(ctx)
		t.Fatalf("migrate: %v", err)
	}
	_ = mc.Close(ctx)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = c.Close(context.Background())
	})
	return pool
}

func mustRecord(t *testing.T, s *Store, m Measurement) {
	t.Helper()
	if err := s.Record(context.Background(), m); err != nil {
		t.Fatalf("record %s: %v", m.RequestID, err)
	}
}

// backdate moves a row's created_at so the window assertions test the WHERE
// clause rather than the wall clock.
func backdate(t *testing.T, pool *pgxpool.Pool, requestID string, age time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE compression_measurements SET created_at = NOW() - $2::interval WHERE request_id = $1`,
		requestID, fmt.Sprintf("%d seconds", int(age.Seconds())))
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

func TestRealPG_TheZeroSavingRowsAreTheDenominator(t *testing.T) {
	pool := measurePool(t)
	s := NewStore(pool)
	ctx := context.Background()

	// Two gated requests that saved nothing, one that saved 20 bytes. The
	// rewriter's measured behaviour is mostly the first kind.
	mustRecord(t, s, Measurement{RequestID: "r1", WorkspaceID: "ws-a", Model: "m", OriginalBytes: 100, SentBytes: 100, Modified: false, BilledInputTokens: 25, CostEstimated: true})
	mustRecord(t, s, Measurement{RequestID: "r2", WorkspaceID: "ws-a", Model: "m", OriginalBytes: 100, SentBytes: 100, Modified: false, BilledInputTokens: 25, CostEstimated: true})
	mustRecord(t, s, Measurement{RequestID: "r3", WorkspaceID: "ws-a", Model: "m", OriginalBytes: 100, SentBytes: 80, Modified: true, BilledInputTokens: 20, CostEstimated: false})

	got, err := s.Summarise(ctx, "ws-a", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if got.Requests != 3 {
		t.Errorf("requests = %d, want 3 — the saving-free rows must be in the denominator", got.Requests)
	}
	if got.Modified != 1 {
		t.Errorf("modified = %d, want 1", got.Modified)
	}
	if got.OriginalBytes != 300 || got.SentBytes != 280 || got.BytesRemoved != 20 {
		t.Errorf("bytes = (orig %d, sent %d, removed %d), want (300, 280, 20)",
			got.OriginalBytes, got.SentBytes, got.BytesRemoved)
	}
	if got.EstimatedPathRequests != 2 {
		t.Errorf("estimated_path_requests = %d, want 2", got.EstimatedPathRequests)
	}
}

// An EMPTY table is a measurement — "0 gated requests" — and must not surface as
// an error or a NULL scan failure. It is a different answer from ErrUnwired, and
// the two are the whole reason the route can distinguish "quiet" from "broken".
func TestRealPG_AnEmptyTableAnswersZeroRequestsWithoutError(t *testing.T) {
	s := NewStore(measurePool(t))
	got, err := s.Summarise(context.Background(), "ws-never-used", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("summarise over an empty table: %v (SUM over zero rows is NULL — COALESCE is load-bearing)", err)
	}
	if got.Requests != 0 || got.BytesRemoved != 0 {
		t.Errorf("empty table summarised as %+v, want zeroes", got)
	}
}

// THE TENANT BOUNDARY IS THE WHERE CLAUSE. Another workspace's rows must be
// invisible, and the control is that they exist at all — a scoping test over an
// empty neighbour proves nothing.
func TestRealPG_AnotherWorkspacesRowsAreInvisible(t *testing.T) {
	pool := measurePool(t)
	s := NewStore(pool)
	ctx := context.Background()

	mustRecord(t, s, Measurement{RequestID: "mine", WorkspaceID: "ws-a", Model: "m", OriginalBytes: 100, SentBytes: 90, Modified: true, BilledInputTokens: 25})
	mustRecord(t, s, Measurement{RequestID: "theirs", WorkspaceID: "ws-b", Model: "m", OriginalBytes: 999, SentBytes: 1, Modified: true, BilledInputTokens: 250})

	mine, err := s.Summarise(ctx, "ws-a", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if mine.Requests != 1 || mine.OriginalBytes != 100 {
		t.Errorf("ws-a summarised as %+v — ws-b's row leaked in", mine)
	}
	theirs, err := s.Summarise(ctx, "ws-b", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if theirs.Requests != 1 || theirs.OriginalBytes != 999 {
		t.Errorf("ws-b summarised as %+v — the control row is not where it was put", theirs)
	}
}

// The window is a real filter, controlled in both directions: the same row is
// visible in a wide window and absent from a narrow one.
func TestRealPG_TheWindowExcludesOlderRows(t *testing.T) {
	pool := measurePool(t)
	s := NewStore(pool)
	ctx := context.Background()

	mustRecord(t, s, Measurement{RequestID: "old", WorkspaceID: "ws-w", Model: "m", OriginalBytes: 100, SentBytes: 100, BilledInputTokens: 25})
	mustRecord(t, s, Measurement{RequestID: "new", WorkspaceID: "ws-w", Model: "m", OriginalBytes: 100, SentBytes: 100, BilledInputTokens: 25})
	backdate(t, pool, "old", 72*time.Hour)

	wide, err := s.Summarise(ctx, "ws-w", time.Now().Add(-96*time.Hour))
	if err != nil {
		t.Fatalf("summarise wide: %v", err)
	}
	if wide.Requests != 2 {
		t.Fatalf("wide window sees %d rows, want 2 — the backdate did not land", wide.Requests)
	}
	narrow, err := s.Summarise(ctx, "ws-w", time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("summarise narrow: %v", err)
	}
	if narrow.Requests != 1 {
		t.Errorf("narrow window sees %d rows, want 1", narrow.Requests)
	}
}

// A RETRIED WRITE CANNOT INFLATE THE DENOMINATOR, and cannot rewrite what was
// already observed about a request that has already been served.
func TestRealPG_ARetriedWriteDoesNotDoubleCount(t *testing.T) {
	pool := measurePool(t)
	s := NewStore(pool)
	ctx := context.Background()

	first := Measurement{RequestID: "dup", WorkspaceID: "ws-d", Model: "m", OriginalBytes: 100, SentBytes: 80, Modified: true, BilledInputTokens: 25}
	mustRecord(t, s, first)
	// The same request id carrying different numbers — a retry that has drifted.
	mustRecord(t, s, Measurement{RequestID: "dup", WorkspaceID: "ws-d", Model: "m", OriginalBytes: 5, SentBytes: 5, Modified: false, BilledInputTokens: 1})

	got, err := s.Summarise(ctx, "ws-d", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if got.Requests != 1 {
		t.Errorf("requests = %d after a duplicate write, want 1", got.Requests)
	}
	if got.OriginalBytes != first.OriginalBytes || got.SentBytes != first.SentBytes {
		t.Errorf("bytes = (%d, %d), want the FIRST measurement (%d, %d) — the retry overwrote it",
			got.OriginalBytes, got.SentBytes, first.OriginalBytes, first.SentBytes)
	}
}
