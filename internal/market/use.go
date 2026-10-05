package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	stripe "github.com/stripe/stripe-go/v81"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/workspace"
)

// use.go — B20.2: USE A LISTING, PAY PER USE, AND THE SELLER EARNS.
//
// A use runs one version of a listing for a buyer workspace, through Lens as the buyer (so the models it
// calls are billed to the buyer as usual). A paid listing's price is then METERED onto the buyer's monthly
// marketplace bill (a Stripe usage-based subscription, internal/billing/market_bill.go) — never taken from
// prepaid credits. When the invoice carrying the use is paid, the use clears and the seller earns their
// share in USD: the price less Talyvor's take (internal/fees, B32.8) from the first dollar; each earning is
// payable after a 14-day holdback (B20.5 pays it out). A seller using their own listing, or a buyer linked to the seller by a
// card or an owner (the single-party detector), is charged nothing and earns the seller nothing.

// Holdback is how long an earning waits after its use clears before it can be paid out.
const Holdback = 14 * 24 * time.Hour

// ulxcPerUSDMicro is how many µLXC make one µUSD at the LXC peg (10 at $0.10).
var ulxcPerUSDMicro = int64(math.Round(1 / economy.LXCUSDValue))

// Charges: what a use costs the buyer.
const (
	ChargeBilled = "billed" // the listing's price, metered onto the buyer's bill
	ChargeFree   = "free"   // the listing is free
	ChargeOwn    = "own"    // the seller used their own listing
	ChargeLinked = "linked" // buyer and seller are one party: a wash trade, charged and earned nothing
)

var (
	// ErrNotRunnable: a kind this version of the marketplace cannot run.
	ErrNotRunnable = errors.New("market: this listing cannot be run")
	// ErrNoModel: nothing names the model to run the listing on.
	ErrNoModel = errors.New(`market: name the model to run this listing on ("model")`)
	// ErrNoBill: a paid use, but no marketplace bill is configured to put it on.
	ErrNoBill = errors.New("market: paid listings cannot be used here: no marketplace bill is configured")
)

// Message is one chat message a use sends to a model.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Runner runs one model call through Lens as the caller.
type Runner interface {
	Run(ctx context.Context, model string, messages []Message) (output string, err error)
}

// Meter puts a billed use on the buyer's monthly marketplace bill. The use's id is the meter event's
// identifier, so a retry never bills it twice.
type Meter interface {
	MeterMarketUse(ctx context.Context, buyerWorkspaceID, useID string, ulxc int64, at time.Time) error
}

// AgentJudge judges an agent's use of a listing against its spending rules (B19.2, B19.14), recording the
// use in the same transaction so its daily and monthly limits count it.
type AgentJudge interface {
	JudgeAgentPurchase(ctx context.Context, workspaceID, agentID, listingID string, amount int64, what string, record func(pgx.Tx) error) error
}

// UseRequest is what a buyer asks of a listing.
type UseRequest struct {
	Version   int               `json:"version"`   // 0: the latest
	Model     string            `json:"model"`     // overrides the artifact's model
	Input     string            `json:"input"`     // the user's message (an agent or a skill)
	Variables map[string]string `json:"variables"` // a prompt's {{variables}}
}

// CaseResult is one evaluation case's outcome.
type CaseResult struct {
	Input    string `json:"input"`
	Expected string `json:"expected"`
	Output   string `json:"output"`
	Passed   bool   `json:"passed"`
}

// Use is one use of a listing, as its buyer sees it.
type Use struct {
	ID         string       `json:"id"`
	ListingID  string       `json:"listing_id"`
	Version    int          `json:"version"`
	Kind       string       `json:"kind"`
	Model      string       `json:"model"`
	Charge     string       `json:"charge"`
	PriceULXC  int64        `json:"price_ulxc"`
	Output     string       `json:"output,omitempty"`
	Cases      []CaseResult `json:"cases,omitempty"`
	UsedAt     time.Time    `json:"used_at"`
	AgentID    string       `json:"agent_id,omitempty"`
	Steps      []StepResult `json:"steps,omitempty"` // a pipeline's steps, in the order they ran (B20.7)
	MeterError string       `json:"-"`
}

// StepResult is one step of a pipeline use: the listing it ran, what it answered, and — when the step is
// another seller's listing — its own use on the buyer's bill.
type StepResult struct {
	Step      int    `json:"step"`
	ListingID string `json:"listing_id"`
	Version   int    `json:"version"`
	Kind      string `json:"kind"`
	Model     string `json:"model"`
	Output    string `json:"output"`
	UseID     string `json:"use_id,omitempty"` // "": the pipeline seller's own listing, covered by the pipeline's price
	Charge    string `json:"charge,omitempty"`
	PriceULXC int64  `json:"price_ulxc,omitempty"`
}

// UseDeps are what a use needs besides the catalog.
type UseDeps struct {
	Runner Runner
	Meter  Meter      // nil: paid listings cannot be used by anyone but their seller
	Agents AgentJudge // nil: no agent keys
}

// Use runs listingID for buyerWorkspaceID (agentID when an agent's key asked; "" otherwise) and records
// the use: its charge is metered onto the buyer's bill once the run has answered.
func (s *Store) Use(ctx context.Context, deps UseDeps, buyerWorkspaceID, agentID, listingID string, req UseRequest) (Use, error) {
	l, artifact, version, err := s.resolve(ctx, buyerWorkspaceID, listingID, req.Version)
	if err != nil {
		return Use{}, err
	}
	var calls []call
	var model string
	var steps []pipelineStep
	if l.Kind == "pipeline" {
		steps, err = s.pipelineSteps(ctx, l, artifact, buyerWorkspaceID, req)
	} else {
		calls, model, err = plan(l.Kind, artifact, req)
	}
	if err != nil {
		return Use{}, err
	}
	u := Use{ID: "use_" + uuid.NewString(), ListingID: l.ID, Version: version, Kind: l.Kind, Model: model, AgentID: agentID}
	if u.Charge, u.PriceULXC, err = s.chargeFor(ctx, l, buyerWorkspaceID); err != nil {
		return Use{}, err
	}
	// Every billed use this makes: the listing's own, and each pipeline step that is another seller's.
	billed := map[string]int64{}
	if u.Charge == ChargeBilled {
		billed[u.ID] = u.PriceULXC
	}
	total := u.PriceULXC
	for _, st := range steps {
		if st.UseID != "" && st.Charge == ChargeBilled {
			billed[st.UseID] = st.PriceULXC
			total += st.PriceULXC
		}
	}
	if len(billed) > 0 && deps.Meter == nil {
		return Use{}, ErrNoBill
	}
	ids := []string{u.ID}

	// The use is recorded before it runs, so an agent's limits count it the moment it is judged; a run that
	// fails removes it again, and only a use that ran is ever metered.
	record := func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, agent_id, price_ulxc, charge)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING used_at`,
			u.ID, l.ID, version, l.WorkspaceID, buyerWorkspaceID, agentID, u.PriceULXC, u.Charge).Scan(&u.UsedAt); err != nil {
			return err
		}
		for _, st := range steps {
			if st.UseID == "" {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, agent_id, price_ulxc, charge, used_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				st.UseID, st.ListingID, st.Version, st.seller, buyerWorkspaceID, agentID, st.PriceULXC, st.Charge, u.UsedAt); err != nil {
				return err
			}
			ids = append(ids, st.UseID)
		}
		return nil
	}
	// An agent's every use is judged — a free one too, since its rules may name the listings it may use. A
	// pipeline is judged once, for all it bills.
	if agentID != "" && deps.Agents != nil {
		what := fmt.Sprintf("market:%s:%s:%d:%d", agentID, l.ID, version, total)
		if err := deps.Agents.JudgeAgentPurchase(ctx, buyerWorkspaceID, agentID, l.ID, total, what, record); err != nil {
			return Use{}, err
		}
	} else if err := pgx.BeginFunc(ctx, s.pool, record); err != nil {
		return Use{}, fmt.Errorf("market: record use: %w", err)
	}

	if l.Kind == "pipeline" {
		err = runPipeline(ctx, deps.Runner, steps, req, &u)
	} else {
		err = run(ctx, deps.Runner, calls, model, &u)
	}
	if err != nil {
		if _, derr := s.pool.Exec(ctx, `DELETE FROM market_uses WHERE id = ANY($1) AND metered_at IS NULL`, ids); derr != nil {
			return Use{}, errors.Join(err, derr)
		}
		return Use{}, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE market_uses SET ran_at = now() WHERE id = ANY($1)`, ids); err != nil {
		return u, fmt.Errorf("market: record use: %w", err)
	}
	for _, id := range ids {
		if price, ok := billed[id]; ok {
			if err := s.meter(ctx, deps.Meter, id, buyerWorkspaceID, price, u.UsedAt); err != nil {
				u.MeterError = err.Error() // the buyer has the answer; MeterPending bills it on its next pass
			}
		}
	}
	return u, nil
}

// chargeFor says what one use of l costs buyer: nothing for their own or a free listing, nothing for a
// seller they share a card or an owner with (a wash trade), else its price.
func (s *Store) chargeFor(ctx context.Context, l Listing, buyer string) (string, int64, error) {
	switch {
	case l.WorkspaceID == buyer:
		return ChargeOwn, 0, nil
	case l.PricePerUseULXC == 0:
		return ChargeFree, 0, nil
	}
	if err := workspace.CheckMoneyWall(ctx, s.pool, buyer, l.WorkspaceID); err != nil {
		return "", 0, err
	}
	linked, err := s.linked(ctx, l.WorkspaceID, buyer)
	if err != nil {
		return "", 0, err
	}
	if linked {
		return ChargeLinked, 0, nil
	}
	return ChargeBilled, l.PricePerUseULXC, nil
}

// A use Stripe refuses waits MeterRefusalBackoff before it is tried again, three times longer after each
// refusal (10m, 30m, 1h30, 4h30); its MaxMeterRefusals-th refusal parks it for an operator.
const (
	MaxMeterRefusals    = 5
	MeterRefusalBackoff = 10 * time.Minute
)

// meterRefusal is Stripe's reason when it answered a meter event with a refusal of that one use — a 4xx
// other than 401/403 (Lens's own key), 409 (a request in flight) and 429 (slow down), which would refuse
// every use alike. "" when it did not: Stripe unreachable, or our side.
func meterRefusal(err error) string {
	var se *stripe.Error
	if !errors.As(err, &se) || se.HTTPStatusCode < 400 || se.HTTPStatusCode >= 500 {
		return ""
	}
	switch se.HTTPStatusCode {
	case 401, 403, 409, 429:
		return ""
	}
	reason := se.Msg
	if se.Code != "" {
		reason = string(se.Code) + ": " + reason
	}
	if reason == "" {
		reason = fmt.Sprintf("Stripe answered %d", se.HTTPStatusCode)
	}
	return reason
}

// meter puts one use on its buyer's bill. A use Stripe refuses records the reason, counts the refusal and
// waits out its backoff before MeterPending tries it again; its MaxMeterRefusals-th refusal parks it.
func (s *Store) meter(ctx context.Context, m Meter, useID, buyer string, ulxc int64, at time.Time) error {
	if err := m.MeterMarketUse(ctx, buyer, useID, ulxc, at); err != nil {
		if reason := meterRefusal(err); reason != "" {
			if _, uerr := s.pool.Exec(ctx, `UPDATE market_uses SET meter_refusals = meter_refusals + 1, meter_refused_reason = $2,
				meter_retry_at = now() + make_interval(secs => $3 * power(3, meter_refusals)),
				meter_parked_at = CASE WHEN meter_refusals + 1 >= $4 THEN now() END
				WHERE id = $1 AND metered_at IS NULL`, useID, reason, MeterRefusalBackoff.Seconds(), MaxMeterRefusals); uerr != nil {
				return errors.Join(err, uerr)
			}
		}
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE market_uses SET metered_at = now() WHERE id = $1 AND metered_at IS NULL`, useID)
	return err
}

// MeterPending bills the uses that ran but whose meter event Stripe has not yet accepted — a Stripe outage
// when they were used. Each keeps its id as the meter event's identifier, so none is billed twice. A use
// Stripe refuses is skipped and every other use still billed; one refused too often stays parked
// (ParkedUses). Stripe unreachable stops the pass: the next one tries again.
func (s *Store) MeterPending(ctx context.Context, m Meter, olderThan time.Duration) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, buyer_workspace_id, price_ulxc, used_at FROM market_uses
		WHERE charge = 'billed' AND metered_at IS NULL AND ran_at IS NOT NULL AND ran_at < now() - make_interval(secs => $1)
		  AND meter_parked_at IS NULL AND (meter_retry_at IS NULL OR meter_retry_at <= now())
		  AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = market_uses.id) -- refunded before it was billed: never billed
		ORDER BY used_at LIMIT 200`, olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("market: pending uses: %w", err)
	}
	type pending struct {
		id, buyer string
		ulxc      int64
		at        time.Time
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.buyer, &p.ulxc, &p.at); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("market: pending uses: %w", err)
	}
	n := 0
	for _, p := range todo {
		if err := s.meter(ctx, m, p.id, p.buyer, p.ulxc, p.at); err != nil {
			if meterRefusal(err) != "" {
				continue
			}
			return n, err
		}
		n++
	}
	return n, nil
}

// ParkedUse is a billed use Stripe refused MaxMeterRefusals times: never on its buyer's bill until an
// operator sees to it.
type ParkedUse struct {
	ID               string    `json:"id"`
	ListingID        string    `json:"listing_id"`
	BuyerWorkspaceID string    `json:"buyer_workspace_id"`
	PriceULXC        int64     `json:"price_ulxc"`
	UsedAt           time.Time `json:"used_at"`
	Refusals         int       `json:"refusals"`
	Reason           string    `json:"reason"` // Stripe's, from its last refusal
	ParkedAt         time.Time `json:"parked_at"`
}

// ParkedUses lists every parked use still off its buyer's bill, most recently parked first.
func (s *Store) ParkedUses(ctx context.Context) ([]ParkedUse, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, listing_id, buyer_workspace_id, price_ulxc, used_at, meter_refusals, meter_refused_reason, meter_parked_at
		FROM market_uses WHERE meter_parked_at IS NOT NULL AND metered_at IS NULL ORDER BY meter_parked_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("market: parked uses: %w", err)
	}
	parked, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ParkedUse, error) {
		var p ParkedUse
		err := r.Scan(&p.ID, &p.ListingID, &p.BuyerWorkspaceID, &p.PriceULXC, &p.UsedAt, &p.Refusals, &p.Reason, &p.ParkedAt)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("market: parked uses: %w", err)
	}
	return parked, nil
}

// ErrNotParked: no parked use by that id still off its buyer's bill.
var ErrNotParked = errors.New("market: no such parked use")

// RetryParkedUse un-parks a use an operator has seen to, so the next MeterPending pass tries it at once.
// Its refusals are kept: Stripe refusing it once more parks it again.
func (s *Store) RetryParkedUse(ctx context.Context, useID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE market_uses SET meter_parked_at = NULL, meter_retry_at = NULL
		WHERE id = $1 AND meter_parked_at IS NOT NULL AND metered_at IS NULL`, useID)
	if err != nil {
		return fmt.Errorf("market: retry parked use: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotParked
	}
	return nil
}

// resolve reads the listing and the version the buyer may use, with its artifact. A held listing is its
// owner's alone to use; a taken-down one is nobody's.
func (s *Store) resolve(ctx context.Context, buyer, listingID string, version int) (Listing, map[string]any, int, error) {
	l, err := scanListing(s.pool.QueryRow(ctx, `SELECT `+listingColumns+` FROM market_listings WHERE id = $1`, listingID))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && hidden(l, buyer)) {
		return Listing{}, nil, 0, ErrNotFound
	}
	if err != nil {
		return Listing{}, nil, 0, fmt.Errorf("market: listing: %w", err)
	}
	if l.ReviewStatus == ReviewTakenDown {
		return Listing{}, nil, 0, ErrTakenDown
	}
	if version == 0 {
		version = l.LatestVersion
	}
	var raw []byte
	err = s.pool.QueryRow(ctx, `SELECT artifact FROM market_listing_versions WHERE listing_id = $1 AND version = $2`, listingID, version).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Listing{}, nil, 0, fmt.Errorf("%w: it has no version %d", ErrNotFound, version)
	}
	if err != nil {
		return Listing{}, nil, 0, fmt.Errorf("market: version: %w", err)
	}
	var artifact map[string]any
	if err := json.Unmarshal(raw, &artifact); err != nil {
		return Listing{}, nil, 0, fmt.Errorf("market: version %d of %s is unreadable: %w", version, listingID, err)
	}
	return l, artifact, version, nil
}

// linked is the single-party detector's question (U6 PR2, poolroyalty.sharedFingerprintSQL): do the two
// workspaces share a captured card fingerprint or an owner key?
func (s *Store) linked(ctx context.Context, a, b string) (bool, error) {
	var linked bool
	err := s.pool.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM workspace_card_fingerprints x JOIN workspace_card_fingerprints y ON x.fingerprint_hash = y.fingerprint_hash
		        WHERE x.workspace_id = $1 AND y.workspace_id = $2)
		OR EXISTS (SELECT 1 FROM workspace_owner_links x JOIN workspace_owner_links y ON x.owner_key = y.owner_key
		        WHERE x.workspace_id = $1 AND y.workspace_id = $2)`, a, b).Scan(&linked)
	if err != nil {
		return false, fmt.Errorf("market: single-party check: %w", err)
	}
	return linked, nil
}

var variable = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// maxCases bounds the model calls one evaluation use makes.
const maxCases = 50

type call struct {
	messages []Message
	expected string
	input    string
}

// plan turns a version's artifact and the buyer's request into the model calls a use makes.
func plan(kind string, artifact map[string]any, req UseRequest) ([]call, string, error) {
	model := req.Model
	if model == "" {
		model, _ = artifact["model"].(string)
	}
	if kind == "pipeline" {
		return nil, "", ErrNotRunnable // a pipeline's steps are planned one at a time (pipeline.go)
	}
	if model == "" {
		return nil, "", ErrNoModel
	}
	str := func(k string) string { v, _ := artifact[k].(string); return v }
	switch kind {
	case "agent", "skill":
		system := str("system_prompt")
		if kind == "skill" {
			system = str("instructions")
		}
		if strings.TrimSpace(req.Input) == "" {
			return nil, "", fmt.Errorf("%w: using a %s needs an input", ErrInvalid, kind)
		}
		return []call{{messages: []Message{{"system", system}, {"user", req.Input}}}}, model, nil
	case "prompt":
		var missing []string
		text := variable.ReplaceAllStringFunc(str("template"), func(m string) string {
			name := variable.FindStringSubmatch(m)[1]
			v, ok := req.Variables[name]
			if !ok {
				missing = append(missing, name)
			}
			return v
		})
		if len(missing) > 0 {
			return nil, "", fmt.Errorf("%w: the prompt needs the variables %s", ErrInvalid, strings.Join(missing, ", "))
		}
		return []call{{messages: []Message{{"user", text}}}}, model, nil
	default: // evaluation: each case's input to the buyer's model, passed when the output contains what it expects
		cases, _ := artifact["cases"].([]any)
		if len(cases) > maxCases {
			return nil, "", fmt.Errorf("%w: an evaluation of more than %d cases cannot be run in one use", ErrInvalid, maxCases)
		}
		var calls []call
		for _, c := range cases {
			m, _ := c.(map[string]any)
			in, _ := m["input"].(string)
			exp, _ := m["expected"].(string)
			calls = append(calls, call{messages: []Message{{"user", in}}, input: in, expected: exp})
		}
		return calls, model, nil
	}
}

func run(ctx context.Context, r Runner, calls []call, model string, u *Use) error {
	for _, c := range calls {
		out, err := r.Run(ctx, model, c.messages)
		if err != nil {
			return err
		}
		if u.Kind != "evaluation" {
			u.Output = out
			continue
		}
		u.Cases = append(u.Cases, CaseResult{Input: c.input, Expected: c.expected, Output: out,
			Passed: c.expected != "" && strings.Contains(strings.ToLower(out), strings.ToLower(c.expected))})
	}
	return nil
}

// ClearInvoice clears the billed uses the buyer's paid marketplace invoice carried — those used within
// [periodStart, periodEnd) — and credits each seller their share. A replay clears nothing more. Each earning
// is live or test as the invoice was (B22.1): only live earnings reach a live payout.
func (s *Store) ClearInvoice(ctx context.Context, buyerWorkspaceID, invoiceID string, periodStart, periodEnd, paidAt time.Time, livemode bool) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, seller_workspace_id, price_ulxc, COALESCE(payee_agent_id, '') <> '' FROM market_uses
			WHERE buyer_workspace_id = $1 AND charge = 'billed' AND metered_at IS NOT NULL AND cleared_at IS NULL
			  AND used_at >= $2 AND used_at < $3
			ORDER BY used_at, id FOR UPDATE`, buyerWorkspaceID, periodStart, periodEnd)
		if err != nil {
			return err
		}
		type cleared struct {
			id, seller string
			ulxc       int64
			payment    bool // a payment to another company's agent, not a use of a listing
		}
		var uses []cleared
		for rows.Next() {
			var c cleared
			if err := rows.Scan(&c.id, &c.seller, &c.ulxc, &c.payment); err != nil {
				rows.Close()
				return err
			}
			uses = append(uses, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range uses {
			// A use refunded before its invoice was paid (its listing was taken down, B20.4) earns nothing: the
			// buyer is credited it on their next bill. Asked under the use's lock, so a refund racing this sees
			// the earning, or this sees the refund.
			var refunded bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM market_refunds WHERE use_id = $1)`, c.id).Scan(&refunded); err != nil {
				return err
			}
			if refunded {
				continue
			}
			gross := c.ulxc / ulxcPerUSDMicro
			share := SellerShare(gross, takeBPS(c.payment))
			if _, err := tx.Exec(ctx, `INSERT INTO market_earnings (use_id, seller_workspace_id, gross_usd_micros, share_usd_micros, fee_usd_micros, invoice_id,
				cleared_at, payable_at, livemode) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, c.id, c.seller, gross, share, gross-share, invoiceID, paidAt,
				paidAt.Add(Holdback), livemode); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE market_uses SET cleared_invoice_id = $2, cleared_at = $3 WHERE id = $1`, c.id, invoiceID, paidAt); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("market: clear invoice %s: %w", invoiceID, err)
	}
	return n, nil
}

// SellerShare is what a seller keeps of gross (µUSD, the price before tax) when Talyvor takes takeBPS of it,
// from the first dollar: gross × (10,000 − takeBPS) ÷ 10,000, rounded down. Talyvor's fee is gross − share, so
// rounding never pays out more than the sale.
func SellerShare(gross, takeBPS int64) int64 {
	keep := fees.BPSDenominator - takeBPS
	return gross/fees.BPSDenominator*keep + gross%fees.BPSDenominator*keep/fees.BPSDenominator
}

// takeBPS is Talyvor's take on a use of a listing, or on a payment to another company's agent (a service).
func takeBPS(payment bool) int64 {
	if payment {
		return fees.Current().ServicesTakeBPS
	}
	return fees.Current().MarketTakeBPS
}

// Earnings is a seller's marketplace earnings, in µUSD.
type Earnings struct {
	// Pending: uses of the seller's listings billed to buyers who have not yet paid that bill — how many,
	// and what the seller's share of them would be today. They become earnings when the invoice is paid.
	PendingUses         int64     `json:"pending_uses"`
	PendingUSDMicros    int64     `json:"pending_usd_micros"`
	PayableUSDMicros    int64     `json:"payable_usd_micros"`     // every share earned and not yet paid out (B20.5)
	InHoldbackUSDMicros int64     `json:"in_holdback_usd_micros"` // of which still inside the 14-day holdback
	AvailableUSDMicros  int64     `json:"available_usd_micros"`   // of which past it
	PaidOutUSDMicros    int64     `json:"paid_out_usd_micros"`    // every payout, in money or credits (B20.5)
	OwedUSDMicros       int64     `json:"owed_usd_micros"`        // reversed after it was paid out: recovered from future earnings
	LifetimeGrossMicros int64     `json:"lifetime_gross_usd_micros"`
	RefundedUSDMicros   int64     `json:"refunded_usd_micros"` // shares reversed by refunds and chargebacks, already out of the totals above
	Earnings            []Earning `json:"earnings"`
}

// Earning is one cleared use's share.
type Earning struct {
	UseID          string     `json:"use_id"`
	ListingID      string     `json:"listing_id"`
	GrossUSDMicros int64      `json:"gross_usd_micros"`
	ShareUSDMicros int64      `json:"share_usd_micros"`
	FeeUSDMicros   int64      `json:"fee_usd_micros"` // Talyvor's take: gross − share (0 on an earning cleared before B32.8)
	InvoiceID      string     `json:"invoice_id"`
	ClearedAt      time.Time  `json:"cleared_at"`
	PayableAt      time.Time  `json:"payable_at"`
	RefundedAt     *time.Time `json:"refunded_at,omitempty"`    // its share was reversed: the listing was taken down
	PayeeAgentID   string     `json:"payee_agent_id,omitempty"` // a payment to this company's agent, not a use of a listing (B19.15)
}

// SellerEarnings reads a seller's earnings: the totals and the latest 100. An earning a refund or chargeback
// reversed counts in none of the totals but RefundedUSDMicros, and every payout comes out of what is
// available; a payout the reversals have since overtaken is owed, out of what the holdback still holds.
func (s *Store) SellerEarnings(ctx context.Context, sellerWorkspaceID string, now time.Time) (Earnings, error) {
	e := Earnings{Earnings: []Earning{}}
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(e.gross_usd_micros), 0)::bigint,
		       COALESCE(sum(r.reversed_share_usd_micros), 0)::bigint
		FROM market_earnings e LEFT JOIN market_refunds r ON r.use_id = e.use_id
		WHERE e.seller_workspace_id = $1`, sellerWorkspaceID).
		Scan(&e.LifetimeGrossMicros, &e.RefundedUSDMicros); err != nil {
		return e, fmt.Errorf("market: earnings: %w", err)
	}
	released, inHoldback, paid, err := sellerBalance(ctx, s.pool, sellerWorkspaceID, now)
	if err != nil {
		return e, fmt.Errorf("market: earnings: %w", err)
	}
	e.InHoldbackUSDMicros, e.PaidOutUSDMicros = inHoldback, paid
	e.AvailableUSDMicros, e.OwedUSDMicros = max(released-paid, 0), max(paid-released, 0)
	e.PayableUSDMicros = max(inHoldback+released-paid, 0)
	var usesULXC, paymentsULXC int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*),
		       COALESCE(sum(price_ulxc) FILTER (WHERE COALESCE(payee_agent_id, '') = ''), 0)::bigint,
		       COALESCE(sum(price_ulxc) FILTER (WHERE COALESCE(payee_agent_id, '') <> ''), 0)::bigint
		FROM market_uses u
		WHERE seller_workspace_id = $1 AND charge = 'billed' AND ran_at IS NOT NULL AND cleared_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = u.id)`,
		sellerWorkspaceID).Scan(&e.PendingUses, &usesULXC, &paymentsULXC); err != nil {
		return e, fmt.Errorf("market: pending earnings: %w", err)
	}
	e.PendingUSDMicros = SellerShare(usesULXC/ulxcPerUSDMicro, takeBPS(false)) + SellerShare(paymentsULXC/ulxcPerUSDMicro, takeBPS(true))
	rows, err := s.pool.Query(ctx, `SELECT e.use_id, COALESCE(u.listing_id, ''), e.gross_usd_micros, e.share_usd_micros, e.fee_usd_micros, e.invoice_id, e.cleared_at, e.payable_at, r.refunded_at,
		       COALESCE(u.payee_agent_id, '')
		FROM market_earnings e LEFT JOIN market_uses u ON u.id = e.use_id LEFT JOIN market_refunds r ON r.use_id = e.use_id
		WHERE e.seller_workspace_id = $1 ORDER BY e.cleared_at DESC, e.use_id LIMIT 100`, sellerWorkspaceID)
	if err != nil {
		return e, fmt.Errorf("market: earnings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var x Earning
		if err := rows.Scan(&x.UseID, &x.ListingID, &x.GrossUSDMicros, &x.ShareUSDMicros, &x.FeeUSDMicros, &x.InvoiceID, &x.ClearedAt, &x.PayableAt, &x.RefundedAt,
			&x.PayeeAgentID); err != nil {
			return e, err
		}
		e.Earnings = append(e.Earnings, x)
	}
	return e, rows.Err()
}

// BillLine is one use on a buyer's marketplace bill.
type BillLine struct {
	UseID     string     `json:"use_id"`
	ListingID string     `json:"listing_id"`
	Title     string     `json:"title"`
	AgentID   string     `json:"agent_id,omitempty"`
	PriceULXC int64      `json:"price_ulxc"`
	UsedAt    time.Time  `json:"used_at"`
	Cleared   *time.Time `json:"cleared_at,omitempty"`
	Refunded  *time.Time `json:"refunded_at,omitempty"` // credited back: the listing was taken down (B20.4)
	// A payment to another company's agent (B19.15): the agent paid, and the memo.
	PayeeAgentID string `json:"payee_agent_id,omitempty"`
	Memo         string `json:"memo,omitempty"`
}

// Bill is a buyer's marketplace uses billed in one month (UTC). The totals are what the buyer owes for
// them: a refunded use is listed, and counts in RefundedULXC instead.
type Bill struct {
	Month          string     `json:"month"`
	TotalULXC      int64      `json:"total_ulxc"`
	TotalUSDMicros int64      `json:"total_usd_micros"`
	RefundedULXC   int64      `json:"refunded_ulxc"`
	Lines          []BillLine `json:"lines"`
}

// MonthBill reads the billed uses buyerWorkspaceID made in month ("2006-01").
func (s *Store) MonthBill(ctx context.Context, buyerWorkspaceID string, month time.Time) (Bill, error) {
	from := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	b := Bill{Month: from.Format("2006-01"), Lines: []BillLine{}}
	rows, err := s.pool.Query(ctx, `SELECT u.id, u.listing_id, COALESCE(l.title, 'Payment to ' || a.name, ''), u.agent_id, u.price_ulxc, u.used_at, u.cleared_at,
		       r.refunded_at, u.payee_agent_id, u.memo
		FROM market_uses u LEFT JOIN market_listings l ON l.id = u.listing_id LEFT JOIN market_refunds r ON r.use_id = u.id
		LEFT JOIN agent_accounts a ON a.id = u.payee_agent_id AND u.payee_agent_id <> ''
		WHERE u.buyer_workspace_id = $1 AND u.charge = 'billed' AND u.ran_at IS NOT NULL AND u.used_at >= $2 AND u.used_at < $3
		ORDER BY u.used_at, u.id`, buyerWorkspaceID, from, from.AddDate(0, 1, 0))
	if err != nil {
		return b, fmt.Errorf("market: bill: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var x BillLine
		if err := rows.Scan(&x.UseID, &x.ListingID, &x.Title, &x.AgentID, &x.PriceULXC, &x.UsedAt, &x.Cleared, &x.Refunded,
			&x.PayeeAgentID, &x.Memo); err != nil {
			return b, err
		}
		if x.Refunded != nil {
			b.RefundedULXC += x.PriceULXC
		} else {
			b.TotalULXC += x.PriceULXC
		}
		b.Lines = append(b.Lines, x)
	}
	b.TotalUSDMicros = b.TotalULXC / ulxcPerUSDMicro
	return b, rows.Err()
}
