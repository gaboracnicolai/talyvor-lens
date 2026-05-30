package eval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Schedule makes a dataset run automatically on a cadence so quality
// regressions are caught without anyone kicking off a manual run. The scheduler
// reuses the codebase's standard goroutine-ticker pattern (see StartScheduler).
type Schedule struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	DatasetID   string     `json:"dataset_id"`
	IntervalSec int        `json:"interval_seconds"`
	Enabled     bool       `json:"enabled"`
	TargetModel string     `json:"target_model,omitempty"`
	LastRunAt   *time.Time `json:"last_run_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// due reports whether the schedule should fire at time now.
func (s Schedule) due(now time.Time) bool {
	if !s.Enabled || s.IntervalSec <= 0 {
		return false
	}
	if s.LastRunAt == nil {
		return true
	}
	return now.Sub(*s.LastRunAt) >= time.Duration(s.IntervalSec)*time.Second
}

const (
	insertScheduleSQL = `INSERT INTO eval_schedules
    (id, workspace_id, dataset_id, interval_seconds, enabled, target_model, last_run_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	selectSchedulesSQL = `SELECT id, workspace_id, dataset_id, interval_seconds, enabled, target_model, last_run_at, created_at
FROM eval_schedules WHERE workspace_id = $1 ORDER BY created_at DESC`

	selectEnabledSchedulesSQL = `SELECT id, workspace_id, dataset_id, interval_seconds, enabled, target_model, last_run_at, created_at
FROM eval_schedules WHERE enabled = true`

	markScheduleRanSQL = `UPDATE eval_schedules SET last_run_at = $2 WHERE id = $1`
)

// CreateSchedule validates and persists a scheduled dataset run.
func (p *Pipeline) CreateSchedule(ctx context.Context, s Schedule) (*Schedule, error) {
	if strings.TrimSpace(s.DatasetID) == "" {
		return nil, errors.New("eval: schedule DatasetID required")
	}
	if s.IntervalSec <= 0 {
		return nil, errors.New("eval: schedule IntervalSec must be > 0")
	}
	if s.WorkspaceID == "" {
		s.WorkspaceID = "default"
	}
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	s.CreatedAt = time.Now().UTC()
	if p.pool != nil {
		if _, err := p.pool.Exec(ctx, insertScheduleSQL,
			s.ID, s.WorkspaceID, s.DatasetID, s.IntervalSec, s.Enabled, s.TargetModel, s.LastRunAt, s.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("eval: insert schedule: %w", err)
		}
	}
	return &s, nil
}

// ListSchedules returns the schedules for a workspace.
func (p *Pipeline) ListSchedules(ctx context.Context, workspaceID string) ([]Schedule, error) {
	if p.pool == nil {
		return nil, nil
	}
	if workspaceID == "" {
		workspaceID = "default"
	}
	rows, err := p.pool.Query(ctx, selectSchedulesSQL, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSchedules(rows)
}

func (p *Pipeline) listEnabledSchedules(ctx context.Context) ([]Schedule, error) {
	if p.pool == nil {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, selectEnabledSchedulesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSchedules(rows)
}

func scanSchedules(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]Schedule, error) {
	var out []Schedule
	for rows.Next() {
		var s Schedule
		if err := rows.Scan(&s.ID, &s.WorkspaceID, &s.DatasetID, &s.IntervalSec,
			&s.Enabled, &s.TargetModel, &s.LastRunAt, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// StartScheduler is the long-lived background loop that fires due dataset runs.
// It follows the codebase's canonical goroutine-ticker pattern: a single
// ticker, select on ctx.Done() and ticker.C, deferred Stop. Wire it in main as
// `go evalPipeline.StartScheduler(ctx, time.Minute)`. It is entirely off the
// request hot path — eval runs are background/on-demand only.
func (p *Pipeline) StartScheduler(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Minute
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.runDueSchedules(ctx)
		}
	}
}

// runDueSchedules runs every enabled schedule whose cadence has elapsed.
func (p *Pipeline) runDueSchedules(ctx context.Context) {
	schedules, err := p.listEnabledSchedules(ctx)
	if err != nil {
		slog.Error("eval: scheduler list failed", slog.String("err", err.Error()))
		return
	}
	now := time.Now().UTC()
	for _, s := range schedules {
		if !s.due(now) {
			continue
		}
		run, err := p.RunEval(ctx, s.WorkspaceID, s.DatasetID, Target{Model: s.TargetModel})
		if err != nil {
			slog.Error("eval: scheduled dataset run failed",
				slog.String("schedule_id", s.ID),
				slog.String("dataset_id", s.DatasetID),
				slog.String("err", err.Error()))
			continue
		}
		if len(run.Regressions) > 0 {
			slog.Error("eval: scheduled run found regressions",
				slog.String("dataset_id", s.DatasetID),
				slog.Int("regressions", len(run.Regressions)),
				slog.String("alert", "Eval dataset regressions detected"))
		}
		if p.pool != nil {
			if _, err := p.pool.Exec(ctx, markScheduleRanSQL, s.ID, now); err != nil {
				slog.Warn("eval: mark schedule ran failed", slog.String("err", err.Error()))
			}
		}
	}
}
