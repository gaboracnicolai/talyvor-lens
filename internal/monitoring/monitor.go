package monitoring

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/economy"
)

// What a monitoring case is: about the workspace, and open until an operator decides it (B30.8).
const (
	CaseKind    = "monitoring"
	CaseOpen    = "open"
	caseSubject = "workspace"
	caseName    = "transaction monitoring"
	caseOutcome = "alert"
)

// Who raised an alert.
const (
	RaisedByMovement = "movement" // the payment's own run, as it was posted
	RaisedByNightly  = "nightly"  // the nightly run over the last day's payments
)

// Rates prices pounds and euros in dollars, for comparing a payment with an agent's approval amount: *ecbrate.Book.
type Rates interface {
	ToUSD(ctx context.Context, amountMinor int64, currency string, at time.Time) (ecbrate.Conversion, error)
}

// Alert is one hit on a monitoring case.
type Alert struct {
	ID          string    `json:"id"`
	CaseID      string    `json:"case_id"`
	WorkspaceID string    `json:"workspace_id"`
	Rule        string    `json:"rule"`
	AgentID     string    `json:"agent_id,omitempty"`
	EntryID     string    `json:"entry_id"`
	Currency    string    `json:"currency"`
	Funding     string    `json:"funding"`
	Summary     string    `json:"summary"`
	Entries     []string  `json:"entries"`
	RaisedBy    string    `json:"raised_by"`
	RaisedAt    time.Time `json:"raised_at"`
}

// Monitor runs the rules over the money ledger and keeps what they find on compliance cases.
type Monitor struct {
	pool     *pgxpool.Pool
	settings Settings
	rates    Rates
}

// New monitors the money ledger in pool with settings; rates prices pounds and euros, and may be nil, when the rule on
// approval amounts judges only dollars and USDC.
func New(pool *pgxpool.Pool, settings Settings, rates Rates) *Monitor {
	return &Monitor{pool: pool, settings: settings, rates: rates}
}

// MoneyMoved runs the rules at the payment entryID once it is posted (economy.MoneyMonitor). A hit opens or extends
// the workspace's monitoring case. An entry that moved nothing in or out through a partner is no payment, and judged
// by nothing.
func (m *Monitor) MoneyMoved(ctx context.Context, workspaceID, entryID string) error {
	var at time.Time
	if err := m.pool.QueryRow(ctx, `SELECT created_at FROM money_entries WHERE workspace_id = $1 AND id = $2`,
		workspaceID, entryID).Scan(&at); err != nil {
		return fmt.Errorf("monitoring: read the payment %s: %w", entryID, err)
	}
	hits, err := m.judge(ctx, workspaceID, at.Add(-m.settings.horizon()), at, func(p Payment) bool { return p.EntryID == entryID })
	if err != nil {
		return err
	}
	_, err = m.raise(ctx, workspaceID, hits, RaisedByMovement)
	return err
}

// Sweep is the nightly run: the rules at every payment posted after since and up to until, for every workspace that
// made one. It answers how many alerts it raised — none for a payment its own run already judged.
func (m *Monitor) Sweep(ctx context.Context, since, until time.Time) (int, error) {
	rows, err := m.pool.Query(ctx, `SELECT DISTINCT workspace_id FROM money_entries WHERE created_at > $1 AND created_at <= $2
		AND counterparty <> '' ORDER BY workspace_id`, since, until)
	if err != nil {
		return 0, fmt.Errorf("monitoring: the workspaces to sweep: %w", err)
	}
	workspaces, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("monitoring: the workspaces to sweep: %w", err)
	}
	raised := 0
	var errs []error
	for _, ws := range workspaces {
		hits, err := m.judge(ctx, ws, since.Add(-m.settings.horizon()), until, func(p Payment) bool { return p.At.After(since) })
		if err == nil {
			var n int
			n, err = m.raise(ctx, ws, hits, RaisedByNightly)
			raised += n
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("workspace %s: %w", ws, err))
		}
	}
	return raised, errors.Join(errs...)
}

// judge runs the rules at workspaceID's payments that target picks, over its payments after from and up to until.
func (m *Monitor) judge(ctx context.Context, workspaceID string, from, until time.Time, target func(Payment) bool) ([]Hit, error) {
	payments, err := m.payments(ctx, workspaceID, from, until)
	if err != nil || len(payments) == 0 {
		return nil, err
	}
	approvals, err := m.approvals(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	byFunding := map[string]*history{}
	var order []string
	for _, p := range payments {
		h := byFunding[p.Funding]
		if h == nil {
			h = &history{approvalUSDMicros: approvals, known: time.Duration(m.settings.HistoryDays) * 24 * time.Hour}
			byFunding[p.Funding] = h
			order = append(order, p.Funding)
		}
		h.payments = append(h.payments, p)
	}
	var hits []Hit
	for _, f := range order {
		h := byFunding[f]
		var targets []int
		for i, p := range h.payments {
			if !target(p) {
				continue
			}
			targets = append(targets, i)
			// Only the rule on approval amounts compares currencies, and only for an agent that has one.
			if p.Direction == "out" && approvals[p.AgentID] > 0 {
				for _, j := range h.within(i, m.settings.UnderApproval.window()) {
					if q := &h.payments[j]; q.usdMicros == 0 && q.Direction == "out" && q.AgentID == p.AgentID {
						q.usdMicros = m.usdMicros(ctx, *q)
					}
				}
			}
		}
		hits = append(hits, m.settings.judge(h, targets)...)
	}
	return hits, nil
}

// payments is workspaceID's payments in or out through a partner after from and up to until, by time.
func (m *Monitor) payments(ctx context.Context, workspaceID string, from, until time.Time) ([]Payment, error) {
	rows, err := m.pool.Query(ctx, `SELECT e.id, e.created_at, e.counterparty, p.currency, p.funding,
			COALESCE(sum(p.amount_minor) FILTER (WHERE a.purpose = 'partner' AND p.amount_minor > 0), 0)::bigint,
			COALESCE(-sum(p.amount_minor) FILTER (WHERE a.purpose = 'partner' AND p.amount_minor < 0), 0)::bigint,
			COALESCE(min(a.agent_id) FILTER (WHERE a.purpose <> 'partner'), '')
		FROM money_entries e
		JOIN money_postings p ON p.entry_id = e.id
		JOIN money_accounts a ON a.id = p.account_id
		WHERE e.workspace_id = $1 AND e.created_at > $2 AND e.created_at <= $3 AND e.counterparty <> ''
		GROUP BY e.id, e.created_at, e.counterparty, p.currency, p.funding
		HAVING bool_or(a.purpose = 'partner')`, workspaceID, from, until)
	if err != nil {
		return nil, fmt.Errorf("monitoring: read the payments: %w", err)
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		var p Payment
		var outMinor, inMinor int64
		if err := rows.Scan(&p.EntryID, &p.At, &p.Counterparty, &p.Currency, &p.Funding, &outMinor, &inMinor, &p.AgentID); err != nil {
			return nil, fmt.Errorf("monitoring: read the payments: %w", err)
		}
		// Money the partner postings put into a partner account left the workspace; money they took out came in.
		if outMinor > 0 {
			q := p
			q.Direction, q.AmountMinor = "out", outMinor
			out = append(out, q)
		}
		if inMinor > 0 {
			p.Direction, p.AmountMinor = "in", inMinor
			out = append(out, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("monitoring: read the payments: %w", err)
	}
	sortPayments(out)
	return out, nil
}

// approvals is each of workspaceID's agents' approval amount, in micro-dollars.
func (m *Monitor) approvals(ctx context.Context, workspaceID string) (map[string]int64, error) {
	rows, err := m.pool.Query(ctx, `SELECT agent_id, approval_above_ulxc FROM agent_rules
		WHERE workspace_id = $1 AND approval_above_ulxc IS NOT NULL`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("monitoring: read the approval amounts: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var agent string
		var ulxc int64
		if err := rows.Scan(&agent, &ulxc); err != nil {
			return nil, fmt.Errorf("monitoring: read the approval amounts: %w", err)
		}
		out[agent] = ulxc / economy.ULXCPerUSDMicro
	}
	return out, rows.Err()
}

// usdMicros prices p in micro-dollars: dollars and USDC as they are, pounds and euros at the reference rate of p's day.
// It is 0 when no rate prices it, and then the rule on approval amounts does not judge p.
func (m *Monitor) usdMicros(ctx context.Context, p Payment) int64 {
	switch p.Currency {
	case economy.CurrencyUSD:
		return p.AmountMinor * 10_000
	case economy.CurrencyUSDC:
		return p.AmountMinor
	}
	if m.rates == nil {
		return 0
	}
	c, err := m.rates.ToUSD(ctx, p.AmountMinor, p.Currency, p.At)
	if err != nil {
		return 0
	}
	return c.USDMicros
}

// raise keeps hits on workspaceID's open monitoring case — opening one for the first — and answers how many it kept.
// A rule judges a payment once: a hit already kept is not kept again.
func (m *Monitor) raise(ctx context.Context, workspaceID string, hits []Hit, by string) (int, error) {
	if len(hits) == 0 {
		return 0, nil
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("monitoring: raise: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One run at a time raises a workspace's alerts, so two payments posted together open one case.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('compliance-monitoring:' || $1, 0))`, workspaceID); err != nil {
		return 0, fmt.Errorf("monitoring: raise: %w", err)
	}
	kept := 0
	caseID := ""
	for _, h := range hits {
		var known bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM compliance_alerts WHERE workspace_id = $1 AND rule = $2
			AND entry_id = $3 AND currency = $4)`, workspaceID, h.Rule, h.Payment.EntryID, h.Payment.Currency).Scan(&known); err != nil {
			return 0, fmt.Errorf("monitoring: raise: %w", err)
		}
		if known {
			continue
		}
		if caseID == "" {
			if caseID, err = openCase(ctx, tx, workspaceID); err != nil {
				return 0, err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO compliance_alerts (id, case_id, workspace_id, rule, agent_id, entry_id, currency, funding,
				summary, entries, raised_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			"cal_"+uuid.NewString(), caseID, workspaceID, h.Rule, h.Payment.AgentID, h.Payment.EntryID, h.Payment.Currency,
			h.Payment.Funding, h.Summary, h.Entries, by); err != nil {
			return 0, fmt.Errorf("monitoring: raise: %w", err)
		}
		kept++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("monitoring: raise: %w", err)
	}
	return kept, nil
}

// openCase is workspaceID's open monitoring case, opened now when it has none.
func openCase(ctx context.Context, tx pgx.Tx, workspaceID string) (string, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO compliance_cases (id, workspace_id, kind, subject_kind, subject_id, name, outcome, status)
		VALUES ($1, $2, $3, $4, $2, $5, $6, $7) ON CONFLICT (workspace_id) WHERE kind = 'monitoring' AND status = 'open' DO NOTHING`,
		"cc_"+uuid.NewString(), workspaceID, CaseKind, caseSubject, caseName, caseOutcome, CaseOpen); err != nil {
		return "", fmt.Errorf("monitoring: open the case: %w", err)
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM compliance_cases WHERE workspace_id = $1 AND kind = $2 AND status = $3`,
		workspaceID, CaseKind, CaseOpen).Scan(&id); err != nil {
		return "", fmt.Errorf("monitoring: open the case: %w", err)
	}
	return id, nil
}

// Alerts is the monitoring alerts, newest first, at most limit: on caseID only when it is given.
func (m *Monitor) Alerts(ctx context.Context, caseID string, limit int) ([]Alert, error) {
	rows, err := m.pool.Query(ctx, `SELECT id, case_id, workspace_id, rule, agent_id, entry_id, currency, funding, summary, entries,
		raised_by, raised_at FROM compliance_alerts WHERE $1 = '' OR case_id = $1 ORDER BY raised_at DESC, id LIMIT $2`, caseID, limit)
	if err != nil {
		return nil, fmt.Errorf("monitoring: alerts: %w", err)
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.CaseID, &a.WorkspaceID, &a.Rule, &a.AgentID, &a.EntryID, &a.Currency, &a.Funding, &a.Summary,
			&a.Entries, &a.RaisedBy, &a.RaisedAt); err != nil {
			return nil, fmt.Errorf("monitoring: alerts: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
