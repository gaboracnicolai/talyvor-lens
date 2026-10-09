// Package compliance is B30.8's compliance case file: an operator reads a case — what opened it, its alerts, the
// owner and the agents involved, its notes and a timeline — freezes or unfreezes the workspace's money capabilities,
// adds notes, closes the case with a reason, and exports a report draft as text for a person to file.
//
// Every action is an operator_audit row naming the case (screening.AuditTarget), written in the transaction that
// does it: a note is such a row, so the notes and the timeline are read from the append-only trail. The cases are
// the ones screening (B30.6) and transaction monitoring (B30.7) open; a held screening case is decided by
// screening.Screener.Decide, which records its row the same way.
package compliance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/monitoring"
	"github.com/talyvor/lens/internal/operatoraudit"
	"github.com/talyvor/lens/internal/screening"
)

// CaseClosed is the status of a case an operator closed.
const CaseClosed = "closed"

// The operator_audit actions on a case.
const (
	ActionNote     = "compliance.case.note"
	ActionFreeze   = "compliance.case.freeze"
	ActionUnfreeze = "compliance.case.unfreeze"
	ActionClose    = "compliance.case.close"
	ActionExport   = "compliance.case.export"
	ActionRelease  = "compliance.case.release" // screening.Screener.Decide
	ActionRefuse   = "compliance.case.refuse"  // screening.Screener.Decide
)

// maxText bounds a note or a reason: it is kept in an audit row's detail, with room for what the row adds.
const maxText = 3500

var (
	// ErrInvalid: a note with no text, or an action with no reason or no operator.
	ErrInvalid = errors.New("compliance: invalid")
	// ErrClosed: the case is closed, and freezes nothing.
	ErrClosed = errors.New("compliance: the case is closed")
	// ErrNotClosable: only an open monitoring case is closed; a held screening case is released or refused.
	ErrNotClosable = errors.New("compliance: only an open transaction-monitoring case is closed; a held screening case is released or refused")
	// ErrAlreadyFrozen: the workspace is frozen already.
	ErrAlreadyFrozen = errors.New("compliance: the workspace is frozen already")
	// ErrNotFrozen: the workspace is not frozen.
	ErrNotFrozen = errors.New("compliance: the workspace is not frozen")
)

// Alerts reads a case's alerts, newest first: *monitoring.Monitor.
type Alerts interface {
	Alerts(ctx context.Context, caseID string, limit int) ([]monitoring.Alert, error)
}

// Store keeps the case file over the compliance tables in pool.
type Store struct {
	pool   *pgxpool.Pool
	alerts Alerts
}

// New is the case file over pool, its alerts read from alerts.
func New(pool *pgxpool.Pool, alerts Alerts) *Store { return &Store{pool: pool, alerts: alerts} }

// Owner is the workspace a case is about, and who its verification found it to be.
type Owner struct {
	WorkspaceID   string `json:"workspace_id"`
	Name          string `json:"name"`
	VerifiedName  string `json:"verified_name,omitempty"`
	Country       string `json:"country,omitempty"`
	CompanyNumber string `json:"company_number,omitempty"`
	TestWorkspace bool   `json:"test_workspace"`
}

// Agent is an agent whose money a case's alerts are about.
type Agent struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Alerts int    `json:"alerts"`
}

// Note is an operator's note on a case.
type Note struct {
	ID   int64     `json:"id"`
	By   string    `json:"by"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// Event is one line of a case's timeline: opened, alert, note, freeze, unfreeze, close, release, refuse, export —
// or, for a case decided before its decisions were in the trail, the status it was decided to.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	By   string    `json:"by,omitempty"`
	Text string    `json:"text"`
}

// File is the case file.
type File struct {
	Case     screening.Case     `json:"case"`
	Owner    Owner              `json:"owner"`
	Agents   []Agent            `json:"agents"`
	Alerts   []monitoring.Alert `json:"alerts"`
	Notes    []Note             `json:"notes"`
	Freeze   *economy.Freeze    `json:"freeze,omitempty"` // the freeze on the workspace now, on this case or another
	Timeline []Event            `json:"timeline"`
}

// Filter narrows the list of cases; "" does not filter.
type Filter struct {
	Status, Kind, WorkspaceID string
}

// Cases is the cases f selects, newest first, at most limit.
func (s *Store) Cases(ctx context.Context, f Filter, limit int) ([]screening.Case, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+screening.CaseColumns+` FROM compliance_cases
		WHERE ($1 = '' OR status = $1) AND ($2 = '' OR kind = $2) AND ($3 = '' OR workspace_id = $3)
		ORDER BY opened_at DESC, id LIMIT $4`, f.Status, f.Kind, f.WorkspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("compliance: cases: %w", err)
	}
	defer rows.Close()
	out := []screening.Case{}
	for rows.Next() {
		c, err := screening.ScanCase(rows)
		if err != nil {
			return nil, fmt.Errorf("compliance: cases: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Freeze is the freeze on workspaceID now, or nil.
func (s *Store) Freeze(ctx context.Context, workspaceID string) (*economy.Freeze, error) {
	return economy.WorkspaceFreeze(ctx, s.pool, workspaceID)
}

// File is the case file of case id.
func (s *Store) File(ctx context.Context, id string) (File, error) {
	c, err := s.caseByID(ctx, s.pool, id, false)
	if err != nil {
		return File{}, err
	}
	f := File{Case: c, Agents: []Agent{}, Alerts: []monitoring.Alert{}, Notes: []Note{}}
	if f.Owner, err = s.owner(ctx, c.WorkspaceID); err != nil {
		return File{}, err
	}
	if c.Kind == monitoring.CaseKind && s.alerts != nil {
		if f.Alerts, err = s.alerts.Alerts(ctx, c.ID, 1000); err != nil {
			return File{}, err
		}
	}
	if f.Agents, err = s.agents(ctx, f.Alerts); err != nil {
		return File{}, err
	}
	if f.Freeze, err = economy.WorkspaceFreeze(ctx, s.pool, c.WorkspaceID); err != nil {
		return File{}, err
	}
	trail, err := s.trail(ctx, c.ID)
	if err != nil {
		return File{}, err
	}
	f.Timeline = []Event{{At: c.OpenedAt, Kind: "opened", Text: opening(c)}}
	for i := len(f.Alerts) - 1; i >= 0; i-- {
		a := f.Alerts[i]
		f.Timeline = append(f.Timeline, Event{At: a.RaisedAt, Kind: "alert", Text: a.Rule + ": " + a.Summary})
	}
	decided := false
	for _, e := range trail {
		kind := strings.TrimPrefix(e.Action, "compliance.case.")
		decided = decided || kind == "release" || kind == "refuse" || kind == "close"
		if e.Action == ActionNote {
			f.Notes = append(f.Notes, Note{ID: e.ID, By: e.Actor, Text: e.Detail, At: e.OccurredAt})
		}
		f.Timeline = append(f.Timeline, Event{At: e.OccurredAt, Kind: kind, By: e.Actor, Text: e.Detail})
	}
	if !decided && c.DecidedAt != nil {
		f.Timeline = append(f.Timeline, Event{At: *c.DecidedAt, Kind: c.Status, By: c.DecidedBy, Text: c.DecisionNote})
	}
	sort.SliceStable(f.Timeline, func(i, j int) bool { return f.Timeline[i].At.Before(f.Timeline[j].At) })
	return f, nil
}

func opening(c screening.Case) string {
	if c.Kind == monitoring.CaseKind {
		return "Transaction monitoring opened the case on the workspace's payments."
	}
	what := "is close to a name"
	if c.Outcome == "hit" {
		what = "matches a name"
	}
	return fmt.Sprintf("Screening found the %s's name %q %s on a sanctions list; the case is %s.", c.SubjectKind, c.Name, what, c.Status)
}

// owner is workspaceID's name and what its highest completed verification confirmed.
func (s *Store) owner(ctx context.Context, workspaceID string) (Owner, error) {
	o := Owner{WorkspaceID: workspaceID}
	err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT name FROM workspaces WHERE id = $1), ''),
		COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`, workspaceID).Scan(&o.Name, &o.TestWorkspace)
	if err != nil {
		return Owner{}, fmt.Errorf("compliance: owner: %w", err)
	}
	err = s.pool.QueryRow(ctx, `SELECT verified_name, country, company_number FROM (
			SELECT DISTINCT ON (evidence_ref) * FROM workspace_verifications WHERE workspace_id = $1 ORDER BY evidence_ref, id DESC) latest
		WHERE status = 'completed' AND verified_name <> '' ORDER BY level DESC, id DESC LIMIT 1`, workspaceID).
		Scan(&o.VerifiedName, &o.Country, &o.CompanyNumber)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Owner{}, fmt.Errorf("compliance: owner: %w", err)
	}
	return o, nil
}

// agents is each agent the alerts are about, with its name, in the order they first appear.
func (s *Store) agents(ctx context.Context, alerts []monitoring.Alert) ([]Agent, error) {
	out := []Agent{}
	at := map[string]int{}
	for _, a := range alerts {
		if a.AgentID == "" {
			continue
		}
		if i, ok := at[a.AgentID]; ok {
			out[i].Alerts++
			continue
		}
		at[a.AgentID] = len(out)
		out = append(out, Agent{ID: a.AgentID, Alerts: 1})
	}
	for i := range out {
		err := s.pool.QueryRow(ctx, `SELECT name FROM agent_accounts WHERE id = $1`, out[i].ID).Scan(&out[i].Name)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("compliance: agents: %w", err)
		}
	}
	return out, nil
}

// trail is every operator_audit row on case id, oldest first.
func (s *Store) trail(ctx context.Context, id string) ([]operatoraudit.Entry, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, actor, action, target, detail, occurred_at, recorded_at FROM operator_audit
		WHERE target = $1 ORDER BY occurred_at, id`, screening.AuditTarget(id))
	if err != nil {
		return nil, fmt.Errorf("compliance: trail: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (operatoraudit.Entry, error) {
		var e operatoraudit.Entry
		return e, row.Scan(&e.ID, &e.Actor, &e.Action, &e.Target, &e.Detail, &e.OccurredAt, &e.RecordedAt)
	})
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// caseByID reads case id, locked for the rest of the transaction when lock is set.
func (s *Store) caseByID(ctx context.Context, q querier, id string, lock bool) (screening.Case, error) {
	sql := `SELECT ` + screening.CaseColumns + ` FROM compliance_cases WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	c, err := screening.ScanCase(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return screening.Case{}, screening.ErrCaseNotFound
	}
	if err != nil {
		return screening.Case{}, fmt.Errorf("compliance: case: %w", err)
	}
	return c, nil
}

// text checks an operator and a note or reason, and trims them.
func text(by, what, s string) (string, string, error) {
	by, s = strings.TrimSpace(by), strings.TrimSpace(s)
	switch {
	case by == "":
		return "", "", fmt.Errorf("%w: an action names the operator who takes it", ErrInvalid)
	case s == "":
		return "", "", fmt.Errorf("%w: %s is required", ErrInvalid, what)
	case len(s) > maxText:
		return "", "", fmt.Errorf("%w: %s is longer than %d bytes", ErrInvalid, what, maxText)
	}
	return by, s, nil
}

// act runs do on case id, locked, in a transaction with its audit row: the action and its row commit together, or
// neither does.
func (s *Store) act(ctx context.Context, id, by, action string, do func(tx pgx.Tx, c screening.Case) (detail string, err error)) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := s.caseByID(ctx, tx, id, true)
	if err != nil {
		return err
	}
	detail, err := do(tx, c)
	if err != nil {
		return err
	}
	if _, err := operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: by, Action: action, Target: screening.AuditTarget(id),
		Detail: detail}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AddNote adds an operator's note to case id.
func (s *Store) AddNote(ctx context.Context, id, by, note string) error {
	by, note, err := text(by, "the note", note)
	if err != nil {
		return err
	}
	return s.act(ctx, id, by, ActionNote, func(pgx.Tx, screening.Case) (string, error) { return note, nil })
}

// FreezeWorkspace freezes the money capabilities of case id's workspace, for reason: from the next movement every
// AMBER and RED capability refuses its money, test or live, and the GREEN ones go on. A movement already under way
// finishes first.
func (s *Store) FreezeWorkspace(ctx context.Context, id, by, reason string) (economy.Freeze, error) {
	by, reason, err := text(by, "a reason", reason)
	if err != nil {
		return economy.Freeze{}, err
	}
	var f economy.Freeze
	err = s.act(ctx, id, by, ActionFreeze, func(tx pgx.Tx, c screening.Case) (string, error) {
		if c.Status == CaseClosed {
			return "", ErrClosed
		}
		if _, err := tx.Exec(ctx, economy.FreezeLockExclusiveSQL, c.WorkspaceID); err != nil {
			return "", err
		}
		f = economy.Freeze{WorkspaceID: c.WorkspaceID, CaseID: c.ID, Reason: reason, FrozenBy: by}
		err := tx.QueryRow(ctx, `INSERT INTO compliance_freezes (workspace_id, case_id, reason, frozen_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT (workspace_id) DO NOTHING RETURNING frozen_at`, c.WorkspaceID, c.ID, reason, by).Scan(&f.FrozenAt)
		if errors.Is(err, pgx.ErrNoRows) {
			cur, ferr := economy.WorkspaceFreeze(ctx, tx, c.WorkspaceID)
			if ferr != nil || cur == nil {
				return "", errors.Join(ErrAlreadyFrozen, ferr)
			}
			return "", fmt.Errorf("%w, on case %s", ErrAlreadyFrozen, cur.CaseID)
		}
		if err != nil {
			return "", fmt.Errorf("compliance: freeze: %w", err)
		}
		return fmt.Sprintf("froze the money capabilities of workspace %s: %s", c.WorkspaceID, reason), nil
	})
	return f, err
}

// UnfreezeWorkspace lifts the freeze on case id's workspace — whichever of its cases it was frozen on — for reason.
func (s *Store) UnfreezeWorkspace(ctx context.Context, id, by, reason string) error {
	by, reason, err := text(by, "a reason", reason)
	if err != nil {
		return err
	}
	return s.act(ctx, id, by, ActionUnfreeze, func(tx pgx.Tx, c screening.Case) (string, error) {
		if _, err := tx.Exec(ctx, economy.FreezeLockExclusiveSQL, c.WorkspaceID); err != nil {
			return "", err
		}
		var on string
		err := tx.QueryRow(ctx, `DELETE FROM compliance_freezes WHERE workspace_id = $1 RETURNING case_id`, c.WorkspaceID).Scan(&on)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFrozen
		}
		if err != nil {
			return "", fmt.Errorf("compliance: unfreeze: %w", err)
		}
		return fmt.Sprintf("unfroze the money capabilities of workspace %s (frozen on case %s): %s", c.WorkspaceID, on, reason), nil
	})
}

// Close closes the open monitoring case id with reason. A freeze on its workspace stands until it is unfrozen.
func (s *Store) Close(ctx context.Context, id, by, reason string) (screening.Case, error) {
	by, reason, err := text(by, "a reason", reason)
	if err != nil {
		return screening.Case{}, err
	}
	var closed screening.Case
	err = s.act(ctx, id, by, ActionClose, func(tx pgx.Tx, c screening.Case) (string, error) {
		switch {
		case c.Status == CaseClosed:
			return "", ErrClosed
		case c.Kind != monitoring.CaseKind || c.Status != monitoring.CaseOpen:
			return "", ErrNotClosable
		}
		var err error
		closed, err = screening.ScanCase(tx.QueryRow(ctx, `UPDATE compliance_cases SET status = $2, decided_by = $3, decided_at = now(),
			decision_note = $4 WHERE id = $1 RETURNING `+screening.CaseColumns, c.ID, CaseClosed, by, reason))
		if err != nil {
			return "", fmt.Errorf("compliance: close: %w", err)
		}
		return reason, nil
	})
	return closed, err
}
