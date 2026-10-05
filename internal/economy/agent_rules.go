package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // active hours are read in any IANA zone; the runtime image carries no zoneinfo

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// agent_rules.go — B19.2: SPENDING RULES PER AGENT, ENFORCED BEFORE THE PROVIDER IS CALLED.
//
// The rules are judged inside agentMovement — the hold or the debit of an agent's key, under the
// agent's row lock — so they bind every seam that spends an agent's balance (the reservation and the
// immediate debit, buffered and streamed), and concurrent requests cannot together pass a daily or
// monthly limit. What the rules need to know about the request beyond its amount — model, provider,
// fingerprint — rides the context (WithAgentRequest). A request above the approval amount files an
// approval (migration 0141) and is refused until the workspace's owner approves it; the approved
// request is then let through once.

// ErrAgentRule wraps every refusal by an agent's rules except the approval amount's.
var ErrAgentRule = errors.New("economy: the agent's spending rules refuse this request")

// ErrAgentRequestRate is the requests-per-minute rule's refusal (B28.302): an ErrAgentRule, which the proxy answers
// 429 rather than 403, because the same request goes through once the minute has room again.
var ErrAgentRequestRate = fmt.Errorf("%w", ErrAgentRule)

// ErrApprovalRequired is the approval amount's refusal; *ApprovalNeededError carries the approval.
var ErrApprovalRequired = errors.New("economy: this request needs a person's approval")

// ErrApprovalNotFound: no such open approval in this workspace.
var ErrApprovalNotFound = errors.New("economy: no such pending approval in this workspace")

// AgentRules are one agent's spending rules. A zero amount, an empty list or an empty window is no rule.
type AgentRules struct {
	MaxPerRequestULXC int64 `json:"max_per_request_ulxc"`
	// HourlyLimitULXC and WeeklyLimitULXC (B28.300) count the clock hour and the week from Monday. Read, they
	// are never nil; nil (absent from the JSON) saves the rules without changing them, like AllowedListings.
	HourlyLimitULXC   *int64   `json:"hourly_limit_ulxc"`
	DailyLimitULXC    int64    `json:"daily_limit_ulxc"`
	WeeklyLimitULXC   *int64   `json:"weekly_limit_ulxc"`
	MonthlyLimitULXC  int64    `json:"monthly_limit_ulxc"`
	ApprovalAboveULXC int64    `json:"approval_above_ulxc"`
	AllowedModels     []string `json:"allowed_models"`
	AllowedProviders  []string `json:"allowed_providers"`
	AllowedListings   []string `json:"allowed_listings"` // marketplace listings it may use (B19.14); empty allows any
	ActiveFrom        string   `json:"active_from"`      // "HH:MM" in Timezone; with ActiveUntil, the hours it may spend
	ActiveUntil       string   `json:"active_until"`     // exclusive; earlier than ActiveFrom means the window crosses midnight
	Timezone          string   `json:"timezone"`         // IANA name; UTC when empty. Days and months are counted in it too
	// PauseOnUnusualSpend pauses the agent when its spend raises an unusual-spend alert (B19.6).
	PauseOnUnusualSpend bool `json:"pause_on_unusual_spend"`
	// ModelDailyLimitsULXC (B28.301) caps what the agent spends on one model, named as it asks for it, in a day.
	// Read, it is never nil; nil (absent from the JSON) saves the rules without changing it. Saved, it replaces
	// the stored caps whole: a model left out, or capped at zero, has no cap.
	ModelDailyLimitsULXC map[string]int64 `json:"model_daily_limits_ulxc"`
	// RequestsPerMinute (B28.302) caps the questions the agent asks in any sixty seconds. Read, it is never nil;
	// nil (absent from the JSON) saves the rules without changing it.
	RequestsPerMinute *int64 `json:"requests_per_minute"`
}

// AgentRequest is what the rules judge about a request besides its amount.
type AgentRequest struct {
	Model       string
	Provider    string
	Fingerprint string    // the key, model and prompt, hashed: what an approval is for
	At          time.Time // zero means now
	Payment     bool      // a payment to another agent (B19.3) or a card purchase (B19.12): no model or provider to judge
	Listing     string    // the marketplace listing a use is of (B19.14)
	Payee       Payee     // who a payment goes to, and Memo why: the approval it files names both (B23.5)
	Memo        string
}

// Payee is who a payment goes to. Name is the one it had when the approval was filed; empty until then
// (namePayee reads it) unless the caller already knows it.
type Payee struct {
	Kind string `json:"kind"` // agent | listing | company | merchant
	ID   string `json:"id"`
	Name string `json:"name"`
}

// namePayee reads p's name when the caller did not give one; a payee that no longer exists keeps none.
func namePayee(ctx context.Context, q pgxDB, p Payee) (Payee, error) {
	query := map[string]string{
		"agent":   `SELECT name FROM agent_accounts WHERE id = $1`,
		"listing": `SELECT title FROM market_listings WHERE id = $1`,
		"company": `SELECT name FROM workspaces WHERE id = $1`,
	}[p.Kind]
	if p.Name != "" || p.ID == "" || query == "" {
		return p, nil
	}
	if err := q.QueryRow(ctx, query, p.ID).Scan(&p.Name); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("economy: payee name: %w", err)
	}
	return p, nil
}

type agentRequestKey struct{}

// WithAgentRequest carries req to the agent's hold or debit made with ctx.
func WithAgentRequest(ctx context.Context, req AgentRequest) context.Context {
	return context.WithValue(ctx, agentRequestKey{}, req)
}

func agentRequestFrom(ctx context.Context) AgentRequest {
	req, _ := ctx.Value(agentRequestKey{}).(AgentRequest)
	if req.At.IsZero() {
		req.At = time.Now()
	}
	return req
}

// ApprovalNeededError is the refusal of a request above the agent's approval amount. ApprovalID is the
// approval filed for it, which the workspace's owner approves or denies.
type ApprovalNeededError struct {
	ApprovalID  string
	AmountULXC  int64
	workspaceID string
	agentID     string
	req         AgentRequest
}

func (e *ApprovalNeededError) Error() string {
	return fmt.Sprintf("this request would cost up to %s LXC, above the agent's approval amount — approval %s must be approved by the workspace's owner before it is retried",
		lxcString(e.AmountULXC), e.ApprovalID)
}

func (e *ApprovalNeededError) Unwrap() error { return ErrApprovalRequired }

func lxcString(ulxc int64) string { return strconv.FormatFloat(float64(ulxc)/1e6, 'f', -1, 64) }

func ruleRefusal(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAgentRule, fmt.Sprintf(format, args...))
}

func clockMinutes(hhmm string) (int, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return 0, fmt.Errorf("%q is not a time of day (HH:MM)", hhmm)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (r AgentRules) location() (*time.Location, error) {
	if r.Timezone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(r.Timezone)
}

// limitOf is a limit that may be absent: absent is no limit.
func limitOf(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func (r AgentRules) validate() error {
	for _, v := range []int64{r.MaxPerRequestULXC, limitOf(r.HourlyLimitULXC), r.DailyLimitULXC, limitOf(r.WeeklyLimitULXC),
		r.MonthlyLimitULXC, r.ApprovalAboveULXC, limitOf(r.RequestsPerMinute)} {
		if v < 0 {
			return errors.New("a limit cannot be negative (0 means no limit)")
		}
	}
	named := map[string]string{}
	for model, v := range r.ModelDailyLimitsULXC {
		key := modelCapKey(model)
		if key == "" {
			return errors.New("a model's daily limit needs the model's name")
		}
		if v < 0 {
			return fmt.Errorf("the daily limit for %q cannot be negative (0 means no limit)", model)
		}
		if other, ok := named[key]; ok {
			return fmt.Errorf("%q and %q are the same model; give it one daily limit", other, model)
		}
		named[key] = model
	}
	if limitOf(r.RequestsPerMinute) > math.MaxInt32 {
		return errors.New("requests_per_minute is too large")
	}
	if (r.ActiveFrom == "") != (r.ActiveUntil == "") {
		return errors.New("active_from and active_until go together")
	}
	if r.ActiveFrom != "" {
		from, err := clockMinutes(r.ActiveFrom)
		if err != nil {
			return err
		}
		until, err := clockMinutes(r.ActiveUntil)
		if err != nil {
			return err
		}
		if from == until {
			return errors.New("active_from and active_until are the same time; leave both empty for all day")
		}
	}
	if _, err := r.location(); err != nil {
		return fmt.Errorf("timezone %q is not an IANA time zone", r.Timezone)
	}
	return nil
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

const agentRulesColumns = `COALESCE(max_per_request_ulxc, 0), COALESCE(daily_limit_ulxc, 0), COALESCE(monthly_limit_ulxc, 0),
	COALESCE(approval_above_ulxc, 0), allowed_models, allowed_providers, COALESCE(active_from, ''), COALESCE(active_until, ''), timezone,
	pause_on_unusual_spend, allowed_listings, COALESCE(hourly_limit_ulxc, 0), COALESCE(weekly_limit_ulxc, 0), model_daily_limits_ulxc,
	COALESCE(requests_per_minute, 0)`

func scanAgentRules(row pgx.Row) (AgentRules, error) {
	var r AgentRules
	var hourly, weekly, rpm int64
	err := row.Scan(&r.MaxPerRequestULXC, &r.DailyLimitULXC, &r.MonthlyLimitULXC, &r.ApprovalAboveULXC,
		&r.AllowedModels, &r.AllowedProviders, &r.ActiveFrom, &r.ActiveUntil, &r.Timezone, &r.PauseOnUnusualSpend, &r.AllowedListings,
		&hourly, &weekly, &r.ModelDailyLimitsULXC, &rpm)
	r.HourlyLimitULXC, r.WeeklyLimitULXC, r.RequestsPerMinute = &hourly, &weekly, &rpm
	return r, err
}

// modelCapSuffix is what a dated snapshot or a -latest alias adds to its model's name.
var modelCapSuffix = regexp.MustCompile(`-([0-9]{8}|[0-9]{4}-[0-9]{2}-[0-9]{2}|latest)$`)

// modelCapKey is the name a per-model daily cap knows a model by (B28.301): lower-case, without a dated
// snapshot's or a -latest alias's suffix, so an agent cannot step around its cap on claude-opus-4-1 by asking
// for Claude-Opus-4-1 or claude-opus-4-1-20250805. Caps are saved, judged and counted under it.
func modelCapKey(model string) string {
	return modelCapSuffix.ReplaceAllString(strings.ToLower(strings.TrimSpace(model)), "")
}

// modelLimitsJSON is the JSON a model's daily caps are saved as, by modelCapKey, with the zeros (no cap) left
// out; nil when absent.
func modelLimitsJSON(limits map[string]int64) (*string, error) {
	if limits == nil {
		return nil, nil
	}
	kept := map[string]int64{}
	for model, v := range limits {
		if v > 0 {
			kept[modelCapKey(model)] = v
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	out := string(b)
	return &out, nil
}

// SetAgentRules replaces an agent's rules — all but AllowedListings, HourlyLimitULXC, WeeklyLimitULXC,
// ModelDailyLimitsULXC and RequestsPerMinute when they are nil (absent from the JSON): a client that predates them
// (B19.14, B28.300, B28.301, B28.302) must not clear an agent's listings or caps by saving its other rules. An empty
// list or a zero clears them.
func (s *DualTokenStore) SetAgentRules(ctx context.Context, workspaceID, agentID string, r AgentRules) (AgentRules, error) {
	if err := r.validate(); err != nil {
		return r, fmt.Errorf("%w: %s", ErrAgentRule, err)
	}
	if r.AllowedModels == nil {
		r.AllowedModels = []string{}
	}
	if r.AllowedProviders == nil {
		r.AllowedProviders = []string{}
	}
	if r.Timezone == "" {
		r.Timezone = "UTC"
	}
	var from, until any
	if r.ActiveFrom != "" {
		from, until = r.ActiveFrom, r.ActiveUntil
	}
	models, err := modelLimitsJSON(r.ModelDailyLimitsULXC)
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	// $14, $15, $16 and $17 are NULL when absent, which keeps the stored caps; a zero clears one.
	var hourly, weekly, rpm int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO agent_rules (agent_id, workspace_id, max_per_request_ulxc, daily_limit_ulxc, monthly_limit_ulxc,
		       approval_above_ulxc, allowed_models, allowed_providers, active_from, active_until, timezone, pause_on_unusual_spend,
		       allowed_listings, hourly_limit_ulxc, weekly_limit_ulxc, model_daily_limits_ulxc, requests_per_minute)
		SELECT id, workspace_id, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, COALESCE($13::text[], '{}'),
		       NULLIF($14::bigint, 0), NULLIF($15::bigint, 0), COALESCE($16::text::jsonb, '{}'), NULLIF($17::integer, 0)
		  FROM agent_accounts WHERE id = $1 AND workspace_id = $2
		ON CONFLICT (agent_id) DO UPDATE SET max_per_request_ulxc = EXCLUDED.max_per_request_ulxc,
		       daily_limit_ulxc = EXCLUDED.daily_limit_ulxc, monthly_limit_ulxc = EXCLUDED.monthly_limit_ulxc,
		       approval_above_ulxc = EXCLUDED.approval_above_ulxc, allowed_models = EXCLUDED.allowed_models,
		       allowed_providers = EXCLUDED.allowed_providers, active_from = EXCLUDED.active_from,
		       active_until = EXCLUDED.active_until, timezone = EXCLUDED.timezone,
		       pause_on_unusual_spend = EXCLUDED.pause_on_unusual_spend, allowed_listings = COALESCE($13::text[], agent_rules.allowed_listings),
		       hourly_limit_ulxc = NULLIF(COALESCE($14::bigint, agent_rules.hourly_limit_ulxc), 0),
		       weekly_limit_ulxc = NULLIF(COALESCE($15::bigint, agent_rules.weekly_limit_ulxc), 0),
		       model_daily_limits_ulxc = COALESCE($16::text::jsonb, agent_rules.model_daily_limits_ulxc),
		       requests_per_minute = NULLIF(COALESCE($17::integer, agent_rules.requests_per_minute), 0), updated_at = now()
		RETURNING COALESCE(hourly_limit_ulxc, 0), COALESCE(weekly_limit_ulxc, 0), model_daily_limits_ulxc, COALESCE(requests_per_minute, 0)`,
		agentID, workspaceID, nullIfZero(r.MaxPerRequestULXC), nullIfZero(r.DailyLimitULXC), nullIfZero(r.MonthlyLimitULXC),
		nullIfZero(r.ApprovalAboveULXC), r.AllowedModels, r.AllowedProviders, from, until, r.Timezone, r.PauseOnUnusualSpend,
		r.AllowedListings, r.HourlyLimitULXC, r.WeeklyLimitULXC, models, r.RequestsPerMinute).Scan(&hourly, &weekly, &r.ModelDailyLimitsULXC, &rpm)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrAgentNotFound
	}
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	r.HourlyLimitULXC, r.WeeklyLimitULXC, r.RequestsPerMinute = &hourly, &weekly, &rpm
	return r, nil
}

// GetAgentRules reads an agent's rules; an agent with none set has none.
func (s *DualTokenStore) GetAgentRules(ctx context.Context, workspaceID, agentID string) (AgentRules, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		agentID, workspaceID).Scan(&exists); err != nil {
		return AgentRules{}, fmt.Errorf("economy: agent rules: %w", err)
	}
	if !exists {
		return AgentRules{}, ErrAgentNotFound
	}
	r, err := scanAgentRules(s.pool.QueryRow(ctx, `SELECT `+agentRulesColumns+` FROM agent_rules WHERE agent_id = $1`, agentID))
	if errors.Is(err, pgx.ErrNoRows) {
		var hourly, weekly, rpm int64
		return AgentRules{HourlyLimitULXC: &hourly, WeeklyLimitULXC: &weekly, RequestsPerMinute: &rpm, ModelDailyLimitsULXC: map[string]int64{},
			AllowedModels: []string{}, AllowedProviders: []string{}, AllowedListings: []string{}, Timezone: "UTC"}, nil
	}
	if err != nil {
		return r, fmt.Errorf("economy: agent rules: %w", err)
	}
	return r, nil
}

// agentPeriodLimit is one of an agent's spending limits over a period: what it may spend since `since`.
type agentPeriodLimit struct {
	ulxc  int64
	since time.Time
	name  string
}

// periodLimits are r's hourly, daily, weekly and monthly limits, their periods begun in loc as of now (in
// loc): the clock hour, the day, the week from Monday and the month. The hour is counted back from now, not
// rebuilt with time.Date: when the clocks go back an hour repeats, and time.Date may answer its other
// occurrence — one in the future, which would count nothing spent.
func (r AgentRules) periodLimits(now time.Time, loc *time.Location) []agentPeriodLimit {
	monday := now.Day() - (int(now.Weekday())+6)%7
	hour := now.Add(-(time.Duration(now.Minute())*time.Minute + time.Duration(now.Second())*time.Second + time.Duration(now.Nanosecond())))
	return []agentPeriodLimit{
		{limitOf(r.HourlyLimitULXC), hour, "hourly"},
		{r.DailyLimitULXC, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc), "daily"},
		{limitOf(r.WeeklyLimitULXC), time.Date(now.Year(), now.Month(), monday, 0, 0, 0, 0, loc), "weekly"},
		{r.MonthlyLimitULXC, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc), "monthly"},
	}
}

// agentSpentSince is what an agent has spent since `since`, as its period limits count it.
func agentSpentSince(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, since time.Time) (int64, error) {
	var spent int64
	// What it paid other agents counts (B19.3), and its card purchases (B19.12); what they paid it does not raise its limit. What it
	// used of paid marketplace listings counts too (B20.2), though it is billed to its company.
	err := tx.QueryRow(ctx, `SELECT (
		(SELECT COALESCE(-sum(amount_ulxc), 0) FROM agent_postings
		 WHERE workspace_id = $1 AND account = $2 AND created_at >= $3
		   AND (kind IN ('spend', 'hold', 'settle', 'release', 'card') OR (kind = 'pay' AND amount_ulxc < 0)))
		+ (SELECT COALESCE(sum(price_ulxc), 0) FROM market_uses
		   WHERE buyer_workspace_id = $1 AND agent_id = $4 AND charge = 'billed' AND used_at >= $3))::bigint`,
		workspaceID, agentAccount(agentID), since, agentID).Scan(&spent)
	return spent, err
}

// modelDailyLimit is r's daily cap on model (B28.301), 0 for none, and when its day began in loc as of now.
func (r AgentRules) modelDailyLimit(model string, now time.Time, loc *time.Location) (int64, time.Time) {
	return r.ModelDailyLimitsULXC[modelCapKey(model)], time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
}

// agentModelSpentSince is what an agent has spent on model since `since`: its questions to it, held or debited,
// net of what their settles and releases gave back — the postings that name the model (B28.301).
func agentModelSpentSince(ctx context.Context, tx pgx.Tx, workspaceID, agentID, model string, since time.Time) (int64, error) {
	var spent int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(-sum(amount_ulxc), 0)::bigint FROM agent_postings
		WHERE workspace_id = $1 AND account = $2 AND model = $3 AND created_at >= $4 AND kind IN ('spend', 'hold', 'settle', 'release')`,
		workspaceID, agentAccount(agentID), modelCapKey(model), since).Scan(&spent)
	return spent, err
}

// agentRequestsSince is how many questions an agent has held or debited since `since` (B28.302): one per
// reservation or claim, counted by its first posting — a debit's later under-charge settle is a 'spend' of the
// same ref, not another question. A settle is looked for no further back than an hour.
func agentRequestsSince(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, since time.Time) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT ref FROM agent_postings
		WHERE workspace_id = $1 AND account = $2 AND kind IN ('hold', 'spend') AND created_at > $3::timestamptz - interval '1 hour'
		GROUP BY ref HAVING min(created_at) > $3) AS questions`,
		workspaceID, agentAccount(agentID), since).Scan(&n)
	return n, err
}

// CheckAgentRules judges an agent key's request that moves no LXC — one answered on its workspace's own
// provider key under BYOK (B27.26) — against the agent's rules: paused, active hours, models, providers. The
// amount rules pass at zero. A key attached to no agent has none. The request rides ctx (WithAgentRequest).
func (s *DualTokenStore) CheckAgentRules(ctx context.Context, scopedKeyID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var agentID, workspaceID string
	err = tx.QueryRow(ctx, `SELECT a.id, a.workspace_id FROM agent_account_keys k JOIN agent_accounts a ON a.id = k.agent_id
		WHERE k.scoped_key_id = $1`, scopedKeyID).Scan(&agentID, &workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("economy: agent of key: %w", err)
	}
	if err := enforceAgentRules(ctx, tx, workspaceID, agentID, 0, "byok"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// enforceAgentRules judges a positive movement of amount µLXC (a hold or a debit, ref its reservation or
// request id) against the agent's rules, inside the movement's transaction with the agent's row locked.
// A paused agent is refused first; a movement the rules let through is then watched for unusual spend (B19.6).
func enforceAgentRules(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, amount int64, ref string) error {
	if err := refuseIfPaused(ctx, tx, agentID); err != nil {
		return err
	}
	r, err := scanAgentRules(tx.QueryRow(ctx, `SELECT `+agentRulesColumns+` FROM agent_rules WHERE agent_id = $1`, agentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return watchAgentSpend(ctx, tx, workspaceID, agentID, amount, agentRequestFrom(ctx).At, false)
	}
	if err != nil {
		return fmt.Errorf("economy: agent rules: %w", err)
	}
	req := agentRequestFrom(ctx)
	what := "request"
	if req.Payment {
		what = "payment"
	}
	loc, err := r.location()
	if err != nil {
		return ruleRefusal("the agent's timezone %q cannot be read", r.Timezone)
	}
	now := req.At.In(loc)
	if r.ActiveFrom != "" {
		from, _ := clockMinutes(r.ActiveFrom)
		until, _ := clockMinutes(r.ActiveUntil)
		m := now.Hour()*60 + now.Minute()
		inside := from <= m && m < until
		if from > until { // the window crosses midnight
			inside = m >= from || m < until
		}
		if !inside {
			return ruleRefusal("the agent may spend only between %s and %s (%s)", r.ActiveFrom, r.ActiveUntil, r.Timezone)
		}
	}
	if len(r.AllowedModels) > 0 && !req.Payment && !slices.Contains(r.AllowedModels, req.Model) {
		return ruleRefusal("the agent may not use the model %q", req.Model)
	}
	if len(r.AllowedProviders) > 0 && !req.Payment && !slices.Contains(r.AllowedProviders, req.Provider) {
		return ruleRefusal("the agent may not use the provider %q", req.Provider)
	}
	if len(r.AllowedListings) > 0 && req.Listing != "" && !slices.Contains(r.AllowedListings, req.Listing) {
		return ruleRefusal("the agent may not use the marketplace listing %q", req.Listing)
	}
	if rpm := limitOf(r.RequestsPerMinute); rpm > 0 && !req.Payment {
		asked, err := agentRequestsSince(ctx, tx, workspaceID, agentID, req.At.Add(-time.Minute))
		if err != nil {
			return fmt.Errorf("economy: agent requests so far: %w", err)
		}
		if asked >= rpm {
			return fmt.Errorf("%w: the agent may make %d requests a minute and has made %d in the last minute; try again shortly",
				ErrAgentRequestRate, rpm, asked)
		}
	}
	if r.MaxPerRequestULXC > 0 && amount > r.MaxPerRequestULXC {
		return ruleRefusal("this %s would cost up to %s LXC; the agent's limit per request is %s LXC",
			what, lxcString(amount), lxcString(r.MaxPerRequestULXC))
	}
	for _, limit := range r.periodLimits(now, loc) {
		if limit.ulxc == 0 {
			continue
		}
		spent, err := agentSpentSince(ctx, tx, workspaceID, agentID, limit.since)
		if err != nil {
			return fmt.Errorf("economy: agent spend so far: %w", err)
		}
		if spent+amount > limit.ulxc {
			return ruleRefusal("the agent has spent %s LXC of its %s limit of %s LXC, and this %s would cost up to %s LXC",
				lxcString(spent), limit.name, lxcString(limit.ulxc), what, lxcString(amount))
		}
	}
	if limit, since := r.modelDailyLimit(req.Model, now, loc); limit > 0 && !req.Payment {
		spent, err := agentModelSpentSince(ctx, tx, workspaceID, agentID, req.Model, since)
		if err != nil {
			return fmt.Errorf("economy: agent spend on the model so far: %w", err)
		}
		if spent+amount > limit {
			return ruleRefusal("the agent has spent %s LXC of its daily limit of %s LXC for the model %q, and this request would cost up to %s LXC",
				lxcString(spent), lxcString(limit), req.Model, lxcString(amount))
		}
	}
	if r.ApprovalAboveULXC > 0 && amount > r.ApprovalAboveULXC {
		if req.Fingerprint == "" {
			return ruleRefusal("this %s needs approval and carries nothing to approve it by", what)
		}
		// An approved request goes through once: the approval is used up in this transaction, so a
		// refusal later in it (the workspace's balance, say) leaves it approved.
		tag, err := tx.Exec(ctx, `UPDATE agent_approvals SET status = 'used', used_ref = $3
			WHERE agent_id = $1 AND fingerprint = $2 AND status = 'approved'`, agentID, req.Fingerprint, ref)
		if err != nil {
			return fmt.Errorf("economy: use approval: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return &ApprovalNeededError{AmountULXC: amount, workspaceID: workspaceID, agentID: agentID, req: req}
		}
	}
	return watchAgentSpend(ctx, tx, workspaceID, agentID, amount, req.At, r.PauseOnUnusualSpend)
}

// fileApproval records the approval a refused request needs — or finds the one already open for it — once
// the refused transaction has rolled back, and returns the refusal naming it.
func (s *DualTokenStore) fileApproval(ctx context.Context, need *ApprovalNeededError) error {
	payee, err := namePayee(ctx, s.pool, need.req.Payee)
	if err != nil {
		return err
	}
	var filed string
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO agent_approvals (id, workspace_id, agent_id, fingerprint, amount_ulxc, model, payee_kind, payee_id, payee_name, memo)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (agent_id, fingerprint) WHERE status IN ('pending', 'approved') DO NOTHING RETURNING id`,
		"apr_"+uuid.NewString(), need.workspaceID, need.agentID, need.req.Fingerprint, need.AmountULXC, need.req.Model,
		payee.Kind, payee.ID, payee.Name, need.req.Memo).Scan(&filed); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("economy: file approval: %w", err)
	}
	if filed != "" {
		s.notifyApproval(need.workspaceID, filed) // B19.16: a retry finds the open one and tells nobody again
	}
	if err := s.pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1 AND fingerprint = $2
		AND status IN ('pending', 'approved')`, need.agentID, need.req.Fingerprint).Scan(&need.ApprovalID); err != nil {
		return fmt.Errorf("economy: read approval: %w", err)
	}
	return need
}

// refusedMovement is what a hold or debit returns when agentMovement refused it: a request that needs
// approval files it (after rolling back, so nothing of the refused movement is kept); anything else as is.
func (s *DualTokenStore) refusedMovement(ctx context.Context, tx pgx.Tx, err error) error {
	var need *ApprovalNeededError
	if !errors.As(err, &need) {
		return err
	}
	_ = tx.Rollback(ctx)
	return s.fileApproval(ctx, need)
}

// AgentApproval is one request that needed a person's approval.
type AgentApproval struct {
	ID         string     `json:"id"`
	AgentID    string     `json:"agent_id"`
	AmountULXC int64      `json:"amount_ulxc"`
	Model      string     `json:"model"`
	Reason     string     `json:"reason,omitempty"` // why the agent asked (B19.9)
	Payee      *Payee     `json:"payee,omitempty"`  // who a payment goes to (B23.5); none for a request to a model
	Memo       string     `json:"memo,omitempty"`   // the payment's memo
	Status     string     `json:"status"`           // pending | approved | denied | used
	CreatedAt  time.Time  `json:"created_at"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
}

const agentApprovalColumns = `id, agent_id, amount_ulxc, model, reason, payee_kind, payee_id, payee_name, memo, status, created_at, decided_at`

func scanAgentApproval(row pgx.Row) (AgentApproval, error) {
	var a AgentApproval
	var p Payee
	err := row.Scan(&a.ID, &a.AgentID, &a.AmountULXC, &a.Model, &a.Reason, &p.Kind, &p.ID, &p.Name, &a.Memo, &a.Status, &a.CreatedAt, &a.DecidedAt)
	if p.Kind != "" {
		a.Payee = &p
	}
	return a, err
}

// ListAgentApprovals reads a workspace's approvals, newest first.
func (s *DualTokenStore) ListAgentApprovals(ctx context.Context, workspaceID string) ([]AgentApproval, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+agentApprovalColumns+` FROM agent_approvals WHERE workspace_id = $1
		ORDER BY created_at DESC, id LIMIT 200`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: approvals: %w", err)
	}
	defer rows.Close()
	out := []AgentApproval{}
	for rows.Next() {
		a, err := scanAgentApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideAgentApproval approves or denies a pending approval.
func (s *DualTokenStore) DecideAgentApproval(ctx context.Context, workspaceID, approvalID string, approve bool) (AgentApproval, error) {
	status := "denied"
	if approve {
		status = "approved"
	}
	a, err := scanAgentApproval(s.pool.QueryRow(ctx, `UPDATE agent_approvals SET status = $3, decided_at = now()
		WHERE id = $1 AND workspace_id = $2 AND status = 'pending' RETURNING `+agentApprovalColumns, approvalID, workspaceID, status))
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrApprovalNotFound
	}
	if err != nil {
		return a, fmt.Errorf("economy: decide approval: %w", err)
	}
	return a, nil
}
