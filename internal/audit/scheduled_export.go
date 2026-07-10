package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ScheduledExport ships token_events off-box on an interval, advancing a persisted
// watermark only on a successful sink POST. Intended to run under the HA leader so
// exactly one instance exports. Reuses the existing webhook exporter.
type ScheduledExport struct {
	pool     *pgxpool.Pool
	exporter *Exporter
	url      string
	log      *slog.Logger
}

// NewScheduledExport builds the loop. An empty url disables it (default off).
func NewScheduledExport(pool *pgxpool.Pool, url string) *ScheduledExport {
	return &ScheduledExport{pool: pool, exporter: New(pool), url: url, log: slog.Default()}
}

// WithHTTPClient overrides the webhook client on the underlying exporter (test seam: the production SSRF
// guard blocks a loopback httptest sink by design). Nil keeps the guarded default. Returns the receiver
// for chaining.
func (s *ScheduledExport) WithHTTPClient(c *http.Client) *ScheduledExport {
	s.exporter.WithHTTPClient(c)
	return s
}

// ExportOnce reads the watermark, exports (watermark, now] to the sink, and
// advances the watermark to `now` ONLY when the sink POST succeeds. On failure the
// watermark is left untouched so the next run re-exports the gap (at-least-once).
func (s *ScheduledExport) ExportOnce(ctx context.Context) error {
	watermark, err := s.readWatermark(ctx)
	if err != nil {
		return fmt.Errorf("audit export: read watermark: %w", err)
	}
	now := time.Now()
	if !now.After(watermark) {
		return nil // clock skew / nothing new
	}
	filter := ExportFilter{StartTime: watermark, EndTime: now} // all workspaces in the window
	if err := s.exporter.ExportWebhook(ctx, s.url, filter); err != nil {
		// Watermark intentionally NOT advanced — the next run re-covers this window.
		return fmt.Errorf("audit export: sink POST failed (watermark not advanced): %w", err)
	}
	if err := s.advanceWatermark(ctx, now); err != nil {
		return fmt.Errorf("audit export: advance watermark: %w", err)
	}
	return nil
}

func (s *ScheduledExport) readWatermark(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.pool.QueryRow(ctx, `SELECT last_exported_at FROM audit_export_state WHERE id = true`).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil // first run — export everything
	}
	return t, err
}

func (s *ScheduledExport) advanceWatermark(ctx context.Context, to time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE audit_export_state SET last_exported_at = $1, updated_at = NOW() WHERE id = true`, to)
	return err
}

// StartLoop runs ExportOnce on a fixed interval until ctx is cancelled. It is meant
// to be called by the HA leader (so exactly one instance exports); an empty URL
// does not start the loop. BLOCKS until ctx is done (the leader owns the goroutine).
func (s *ScheduledExport) StartLoop(ctx context.Context, interval time.Duration) {
	if s.url == "" {
		s.log.Info("audit off-box export disabled (LENS_AUDIT_EXPORT_URL empty)")
		return
	}
	s.log.Info("audit off-box export loop started", "interval", interval.String())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.ExportOnce(ctx); err != nil {
				s.log.Warn("audit off-box export failed", "err", err)
			}
		}
	}
}
