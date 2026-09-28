package economy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// agent_schedules.go — B19.8: SCHEDULED PAYMENTS AND AUTOMATIC TOP-UPS.
//
// RunAgentSchedules is called on a tick (cmd/lens, every minute). It runs every schedule tick that is
// due, each exactly once (migration 0144) — ticks missed while Lens was down are each run once when it
// is back — and then tops up every agent whose balance is below its threshold. A scheduled payment is an
// ordinary agent payment (B19.3), judged by the payer's rules; a tick they refuse is recorded as refused
// and not retried. A top-up fires once per crossing: under the agent's row lock it brings the balance
// back up to its target, so the next run finds it above the threshold.

// ErrScheduleNotFound: no such payment schedule in this workspace.
var ErrScheduleNotFound = errors.New("economy: no such payment schedule in this workspace")

// maxTicksPerRun bounds one run's catching up; the rest waits for the next run.
const maxTicksPerRun = 1000

// AgentSchedule is a recurring payment from one of a workspace's agents to another.
type AgentSchedule struct {
	ID          string    `json:"id"`
	FromAgentID string    `json:"from_agent_id"`
	ToAgentID   string    `json:"to_agent_id"`
	AmountULXC  int64     `json:"amount_ulxc"`
	Memo        string    `json:"memo,omitempty"`
	Every       string    `json:"every"` // hour | day | week | month
	NextRunAt   time.Time `json:"next_run_at"`
	Active      bool      `json:"active"`
	CreatedAt   time.Time `json:"created_at"`
}

// AgentScheduleRun is one tick of a schedule: paid (EntryID is the payment's entry) or refused (Detail says why).
type AgentScheduleRun struct {
	TickAt    time.Time `json:"tick_at"`
	Outcome   string    `json:"outcome"` // paid | refused
	EntryID   string    `json:"entry_id,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// AgentTopUp is an agent's automatic top-up: below BelowULXC, back up to ToULXC.
type AgentTopUp struct {
	AgentID   string `json:"agent_id"`
	BelowULXC int64  `json:"below_ulxc"`
	ToULXC    int64  `json:"to_ulxc"`
}

// ScheduleRunResult counts what one RunAgentSchedules did.
type ScheduleRunResult struct {
	Paid, Refused, ToppedUp int
}

func nextTick(t time.Time, every string) time.Time {
	switch every {
	case "hour":
		return t.Add(time.Hour)
	case "day":
		return t.AddDate(0, 0, 1)
	case "week":
		return t.AddDate(0, 0, 7)
	default: // month
		return t.AddDate(0, 1, 0)
	}
}

const agentScheduleColumns = `id, from_agent_id, to_agent_id, amount_ulxc, memo, every, next_run_at, active, created_at`

func scanAgentSchedule(row pgx.Row) (AgentSchedule, error) {
	var sc AgentSchedule
	err := row.Scan(&sc.ID, &sc.FromAgentID, &sc.ToAgentID, &sc.AmountULXC, &sc.Memo, &sc.Every, &sc.NextRunAt, &sc.Active, &sc.CreatedAt)
	return sc, err
}

// CreateAgentSchedule schedules fromAgentID to pay toAgentID amount µLXC every hour, day, week or month,
// the first time at firstRunAt.
func (s *DualTokenStore) CreateAgentSchedule(ctx context.Context, workspaceID, fromAgentID, toAgentID string, amount int64,
	memo, every string, firstRunAt time.Time) (AgentSchedule, error) {
	switch {
	case amount <= 0:
		return AgentSchedule{}, fmt.Errorf("%w: the amount must be positive", ErrAgentRule)
	case every != "hour" && every != "day" && every != "week" && every != "month":
		return AgentSchedule{}, fmt.Errorf("%w: every must be hour, day, week or month", ErrAgentRule)
	case fromAgentID == toAgentID:
		return AgentSchedule{}, ErrSameAgent
	}
	sc, err := scanAgentSchedule(s.pool.QueryRow(ctx, `
		INSERT INTO agent_payment_schedules (id, workspace_id, from_agent_id, to_agent_id, amount_ulxc, memo, every, next_run_at)
		SELECT $1, $2, f.id, t.id, $5, $6, $7, $8 FROM agent_accounts f, agent_accounts t
		 WHERE f.id = $3 AND f.workspace_id = $2 AND t.id = $4 AND t.workspace_id = $2
		RETURNING `+agentScheduleColumns,
		"sch_"+uuid.NewString(), workspaceID, fromAgentID, toAgentID, amount, memo, every, firstRunAt.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		return sc, ErrAgentNotFound
	}
	if err != nil {
		return sc, fmt.Errorf("economy: create schedule: %w", err)
	}
	return sc, nil
}

// ListAgentSchedules reads a workspace's schedules, active first, then newest.
func (s *DualTokenStore) ListAgentSchedules(ctx context.Context, workspaceID string) ([]AgentSchedule, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+agentScheduleColumns+` FROM agent_payment_schedules WHERE workspace_id = $1
		ORDER BY active DESC, created_at DESC, id LIMIT 500`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: schedules: %w", err)
	}
	defer rows.Close()
	out := []AgentSchedule{}
	for rows.Next() {
		sc, err := scanAgentSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ListAgentScheduleRuns reads a schedule's runs, newest first.
func (s *DualTokenStore) ListAgentScheduleRuns(ctx context.Context, workspaceID, scheduleID string) ([]AgentScheduleRun, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_payment_schedules WHERE id = $1 AND workspace_id = $2)`,
		scheduleID, workspaceID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("economy: schedule runs: %w", err)
	}
	if !exists {
		return nil, ErrScheduleNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT tick_at, outcome, COALESCE(entry_id::text, ''), detail, created_at FROM agent_schedule_runs
		WHERE schedule_id = $1 ORDER BY tick_at DESC LIMIT 500`, scheduleID)
	if err != nil {
		return nil, fmt.Errorf("economy: schedule runs: %w", err)
	}
	defer rows.Close()
	out := []AgentScheduleRun{}
	for rows.Next() {
		var r AgentScheduleRun
		if err := rows.Scan(&r.TickAt, &r.Outcome, &r.EntryID, &r.Detail, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CancelAgentSchedule stops a schedule; its runs are kept.
func (s *DualTokenStore) CancelAgentSchedule(ctx context.Context, workspaceID, scheduleID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE agent_payment_schedules SET active = false WHERE id = $1 AND workspace_id = $2`, scheduleID, workspaceID)
	if err != nil {
		return fmt.Errorf("economy: cancel schedule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrScheduleNotFound
	}
	return nil
}

// SetAgentTopUp sets an agent's automatic top-up: below belowULXC, back up to toULXC.
func (s *DualTokenStore) SetAgentTopUp(ctx context.Context, workspaceID, agentID string, belowULXC, toULXC int64) (AgentTopUp, error) {
	t := AgentTopUp{AgentID: agentID, BelowULXC: belowULXC, ToULXC: toULXC}
	if belowULXC <= 0 || toULXC <= belowULXC {
		return t, fmt.Errorf("%w: a top-up needs 0 < below_ulxc < to_ulxc", ErrAgentRule)
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO agent_topups (agent_id, workspace_id, below_ulxc, to_ulxc)
		SELECT id, workspace_id, $3, $4 FROM agent_accounts WHERE id = $1 AND workspace_id = $2
		ON CONFLICT (agent_id) DO UPDATE SET below_ulxc = EXCLUDED.below_ulxc, to_ulxc = EXCLUDED.to_ulxc, updated_at = now()`,
		agentID, workspaceID, belowULXC, toULXC)
	if err != nil {
		return t, fmt.Errorf("economy: set top-up: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return t, ErrAgentNotFound
	}
	return t, nil
}

// GetAgentTopUp reads an agent's top-up; ok is false when it has none.
func (s *DualTokenStore) GetAgentTopUp(ctx context.Context, workspaceID, agentID string) (t AgentTopUp, ok bool, err error) {
	t.AgentID = agentID
	err = s.pool.QueryRow(ctx, `SELECT below_ulxc, to_ulxc FROM agent_topups WHERE agent_id = $1 AND workspace_id = $2`,
		agentID, workspaceID).Scan(&t.BelowULXC, &t.ToULXC)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, false, nil
	}
	return t, err == nil, err
}

// RemoveAgentTopUp removes an agent's top-up.
func (s *DualTokenStore) RemoveAgentTopUp(ctx context.Context, workspaceID, agentID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM agent_topups WHERE agent_id = $1 AND workspace_id = $2`, agentID, workspaceID); err != nil {
		return fmt.Errorf("economy: remove top-up: %w", err)
	}
	return nil
}

// RunAgentSchedules runs every schedule tick due at now, each exactly once, then every top-up due.
func (s *DualTokenStore) RunAgentSchedules(ctx context.Context, now time.Time) (ScheduleRunResult, error) {
	var res ScheduleRunResult
	for i := 0; i < maxTicksPerRun; i++ {
		outcome, err := s.runScheduleTick(ctx, now)
		if err != nil {
			return res, err
		}
		if outcome == "" {
			break
		}
		if outcome == "paid" {
			res.Paid++
		} else {
			res.Refused++
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT workspace_id, agent_id FROM agent_topups`)
	if err != nil {
		return res, fmt.Errorf("economy: top-ups: %w", err)
	}
	var due [][2]string
	for rows.Next() {
		var ws, agent string
		if err := rows.Scan(&ws, &agent); err != nil {
			rows.Close()
			return res, err
		}
		due = append(due, [2]string{ws, agent})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("economy: top-ups: %w", err)
	}
	for _, d := range due {
		topped, err := s.topUpAgent(ctx, d[0], d[1])
		if err != nil {
			return res, err
		}
		if topped > 0 {
			res.ToppedUp++
		}
	}
	return res, nil
}

// runScheduleTick runs the earliest due tick of any schedule, in one transaction holding the schedule's
// row: the payment, its run row and the next tick. It returns "" when nothing is due.
func (s *DualTokenStore) runScheduleTick(ctx context.Context, now time.Time) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var workspaceID string
	var sc AgentSchedule
	err = tx.QueryRow(ctx, `SELECT workspace_id, id, from_agent_id, to_agent_id, amount_ulxc, memo, every, next_run_at
		FROM agent_payment_schedules WHERE active AND next_run_at <= $1 ORDER BY next_run_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now).
		Scan(&workspaceID, &sc.ID, &sc.FromAgentID, &sc.ToAgentID, &sc.AmountULXC, &sc.Memo, &sc.Every, &sc.NextRunAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("economy: due schedule: %w", err)
	}
	payCtx := WithAgentRequest(ctx, AgentRequest{Payment: true, At: now,
		Fingerprint: "schedule:" + sc.ID + ":" + sc.NextRunAt.UTC().Format(time.RFC3339)})
	// The payment in a savepoint: a refusal is undone and recorded, and the tick still counts as run.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return "", err
	}
	outcome, entryID, detail := "paid", any(nil), ""
	entry := uuid.New()
	_, perr := payAgentTx(payCtx, sp, workspaceID,
		AgentPayment{FromAgentID: sc.FromAgentID, ToAgentID: sc.ToAgentID, AmountULXC: sc.AmountULXC, Memo: sc.Memo}, entry)
	switch {
	case perr == nil:
		if err := sp.Commit(ctx); err != nil {
			return "", err
		}
		entryID = entry
	case errors.Is(perr, ErrAgentFunds), errors.Is(perr, ErrAgentRule), errors.Is(perr, ErrApprovalRequired), errors.Is(perr, ErrAgentNotFound):
		if err := sp.Rollback(ctx); err != nil {
			return "", err
		}
		outcome, detail = "refused", perr.Error()
	default:
		return "", perr
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_schedule_runs (schedule_id, tick_at, outcome, entry_id, detail) VALUES ($1, $2, $3, $4, $5)`,
		sc.ID, sc.NextRunAt, outcome, entryID, detail); err != nil {
		return "", fmt.Errorf("economy: record schedule run: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_payment_schedules SET next_run_at = $2 WHERE id = $1`, sc.ID, nextTick(sc.NextRunAt.UTC(), sc.Every)); err != nil {
		return "", fmt.Errorf("economy: advance schedule: %w", err)
	}
	return outcome, tx.Commit(ctx)
}

// topUpAgent brings an agent below its top-up threshold back up to its target, from the workspace's
// unallocated LXC (as much as there is). It returns what it moved.
func (s *DualTokenStore) topUpAgent(ctx context.Context, workspaceID, agentID string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Lock order everywhere: the agent, then the workspace balance.
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return 0, err
	}
	var below, to int64
	err = tx.QueryRow(ctx, `SELECT below_ulxc, to_ulxc FROM agent_topups WHERE agent_id = $1`, agentID).Scan(&below, &to)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("economy: top-up: %w", err)
	}
	bal, err := accountBalance(ctx, tx, workspaceID, agentAccount(agentID))
	if err != nil || bal >= below {
		return 0, err
	}
	wsBal, _, _, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return 0, err
	}
	wsSide, err := accountBalance(ctx, tx, workspaceID, "workspace")
	if err != nil {
		return 0, err
	}
	spent, err := accountBalance(ctx, tx, workspaceID, "spend")
	if err != nil {
		return 0, err
	}
	// Allocated = Σ agent balances = −(workspace side) − spend, since every entry sums to zero.
	amount := min(to-bal, wsBal-(-wsSide-spent))
	if amount <= 0 {
		return 0, nil
	}
	if err := postEntry(ctx, tx, workspaceID, "topup", "", leg{"workspace", -amount}, leg{agentAccount(agentID), amount}); err != nil {
		return 0, err
	}
	return amount, tx.Commit(ctx)
}
