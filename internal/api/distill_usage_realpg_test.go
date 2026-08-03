package api

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

// ⚠ THE TENANT BOUNDARY IS THE WHOLE POINT OF THIS ENDPOINT EXISTING.
//
// The obvious source for "how much distilling happened" is /v1/api/distill/summary, and it is the
// wrong one: those counters are PROCESS-GLOBAL, so a customer screen fed from them would show
// numbers produced by other companies' documents. This reads token_events, which carries
// workspace_id — so the scoping lives in the WHERE clause and not in a caller remembering to filter.
//
// Against real Postgres, because the claim is about what a query returns.

func usagePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_distusage_%d", time.Now().UnixNano())
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

func seedDistillEvent(t *testing.T, pool *pgxpool.Pool, ws, method string, age time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO token_events (workspace_id, provider, model, input_tokens, output_tokens, distill_method, created_at)
		 VALUES ($1,'anthropic','claude-sonnet-5',100,50,$2, NOW() - $3::interval)`,
		ws, method, fmt.Sprintf("%d seconds", int(age.Seconds())))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// ⚠ ANOTHER TENANT'S DOCUMENTS MUST NOT APPEAR IN THIS COUNT.
func TestDistillUsage_CountsOnlyTheAskingWorkspace(t *testing.T) {
	pool := usagePool(t)
	store := NewDistillUsageStore(pool)

	seedDistillEvent(t, pool, "ws-mine", "convert", time.Hour)
	seedDistillEvent(t, pool, "ws-mine", "convert", time.Hour)
	seedDistillEvent(t, pool, "ws-other", "convert", time.Hour)
	seedDistillEvent(t, pool, "ws-other", "vision_ocr", time.Hour)

	got, err := ReadDistillUsage(context.Background(), store, "ws-mine", 30, time.Now())
	if err != nil {
		t.Fatalf("ReadDistillUsage: %v", err)
	}
	if got.Converted != 2 {
		t.Errorf("converted = %d, want 2 — another company's document count reached a customer's screen", got.Converted)
	}
	if got.VisionOCR != 0 {
		t.Errorf("vision_ocr = %d, want 0 — ws-other's OCR row was counted for ws-mine", got.VisionOCR)
	}
}

// convert and vision_ocr are different things — one is a saving, one is a cost — and must not blend.
func TestDistillUsage_SeparatesConvertFromVisionOCR(t *testing.T) {
	pool := usagePool(t)
	store := NewDistillUsageStore(pool)
	seedDistillEvent(t, pool, "ws1", "convert", time.Hour)
	seedDistillEvent(t, pool, "ws1", "vision_ocr", time.Hour)
	seedDistillEvent(t, pool, "ws1", "", time.Hour) // ordinary traffic — must count as neither

	got, err := ReadDistillUsage(context.Background(), store, "ws1", 30, time.Now())
	if err != nil {
		t.Fatalf("ReadDistillUsage: %v", err)
	}
	if got.Converted != 1 || got.VisionOCR != 1 {
		t.Errorf("converted=%d vision=%d, want 1/1 (and the undistilled row counted as neither)", got.Converted, got.VisionOCR)
	}
}

// The window must actually bound the count, or "this month" is a decoration.
func TestDistillUsage_ExcludesRowsOutsideTheWindow(t *testing.T) {
	pool := usagePool(t)
	store := NewDistillUsageStore(pool)
	seedDistillEvent(t, pool, "ws1", "convert", time.Hour)
	seedDistillEvent(t, pool, "ws1", "convert", 40*24*time.Hour) // older than 30 days

	got, err := ReadDistillUsage(context.Background(), store, "ws1", 30, time.Now())
	if err != nil {
		t.Fatalf("ReadDistillUsage: %v", err)
	}
	if got.Converted != 1 {
		t.Errorf("converted = %d, want 1 — the 40-day-old row was inside a 30-day window", got.Converted)
	}
}

// ⚠ AN UNCONFIGURED READER MUST NOT LOOK LIKE A QUIET WORKSPACE. Zero-because-nothing-happened and
// zero-because-nothing-is-wired are different answers and the screen must be able to tell them apart.
func TestReadDistillUsage_NilStoreIsAnError(t *testing.T) {
	if _, err := ReadDistillUsage(context.Background(), nil, "ws1", 30, time.Now()); err == nil {
		t.Error("a nil store returned no error — an unwired reader would render as '0 documents converted'")
	}
}
