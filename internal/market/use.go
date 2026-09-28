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

	"github.com/talyvor/lens/internal/economy"
)

// use.go — B20.2: USE A LISTING, PAY PER USE, AND THE SELLER EARNS.
//
// A use runs one version of a listing for a buyer workspace, through Lens as the buyer (so the models it
// calls are billed to the buyer as usual). A paid listing's price is then METERED onto the buyer's monthly
// marketplace bill (a Stripe usage-based subscription, internal/billing/market_bill.go) — never taken from
// prepaid credits. When the invoice carrying the use is paid, the use clears and the seller earns their
// share in USD: 100% of the first US$1M they earn, then 85%; each earning is payable after a 14-day
// holdback (B20.5 pays it out). A seller using their own listing, or a buyer linked to the seller by a
// card or an owner (the single-party detector), is charged nothing and earns the seller nothing.

// Holdback is how long an earning waits after its use clears before it can be paid out.
const Holdback = 14 * 24 * time.Hour

// FullShareUpToUSDMicros is the lifetime gross a seller keeps in full (US$1M); past it they keep 85%.
const (
	FullShareUpToUSDMicros = 1_000_000 * 1_000_000
	reducedSharePercent    = 85
)

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
	// ErrNotRunnable: a kind this version of the marketplace cannot run yet.
	ErrNotRunnable = errors.New("market: a pipeline listing cannot be used yet")
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
	MeterError string       `json:"-"`
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
	calls, model, err := plan(l.Kind, artifact, req)
	if err != nil {
		return Use{}, err
	}
	u := Use{ID: "use_" + uuid.NewString(), ListingID: l.ID, Version: version, Kind: l.Kind, Model: model, AgentID: agentID}
	switch {
	case l.WorkspaceID == buyerWorkspaceID:
		u.Charge = ChargeOwn
	case l.PricePerUseULXC == 0:
		u.Charge = ChargeFree
	default:
		linked, err := s.linked(ctx, l.WorkspaceID, buyerWorkspaceID)
		if err != nil {
			return Use{}, err
		}
		if linked {
			u.Charge = ChargeLinked
		} else {
			u.Charge, u.PriceULXC = ChargeBilled, l.PricePerUseULXC
		}
	}
	if u.Charge == ChargeBilled && deps.Meter == nil {
		return Use{}, ErrNoBill
	}

	// The use is recorded before it runs, so an agent's limits count it the moment it is judged; a run that
	// fails removes it again, and only a use that ran is ever metered.
	record := func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, agent_id, price_ulxc, charge)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING used_at`,
			u.ID, l.ID, version, l.WorkspaceID, buyerWorkspaceID, agentID, u.PriceULXC, u.Charge).Scan(&u.UsedAt)
	}
	// An agent's every use is judged — a free one too, since its rules may name the listings it may use.
	if agentID != "" && deps.Agents != nil {
		what := fmt.Sprintf("market:%s:%s:%d:%d", agentID, l.ID, version, u.PriceULXC)
		if err := deps.Agents.JudgeAgentPurchase(ctx, buyerWorkspaceID, agentID, l.ID, u.PriceULXC, what, record); err != nil {
			return Use{}, err
		}
	} else if err := pgx.BeginFunc(ctx, s.pool, record); err != nil {
		return Use{}, fmt.Errorf("market: record use: %w", err)
	}

	if err := run(ctx, deps.Runner, calls, model, &u); err != nil {
		if _, derr := s.pool.Exec(ctx, `DELETE FROM market_uses WHERE id = $1 AND metered_at IS NULL`, u.ID); derr != nil {
			return Use{}, errors.Join(err, derr)
		}
		return Use{}, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE market_uses SET ran_at = now() WHERE id = $1`, u.ID); err != nil {
		return u, fmt.Errorf("market: record use: %w", err)
	}
	if u.Charge == ChargeBilled {
		if err := s.meter(ctx, deps.Meter, u.ID, buyerWorkspaceID, u.PriceULXC, u.UsedAt); err != nil {
			u.MeterError = err.Error() // the buyer has the answer; MeterPending bills it on its next pass
		}
	}
	return u, nil
}

func (s *Store) meter(ctx context.Context, m Meter, useID, buyer string, ulxc int64, at time.Time) error {
	if err := m.MeterMarketUse(ctx, buyer, useID, ulxc, at); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE market_uses SET metered_at = now() WHERE id = $1 AND metered_at IS NULL`, useID)
	return err
}

// MeterPending bills the uses that ran but whose meter event Stripe has not yet accepted — a Stripe outage
// when they were used. Each keeps its id as the meter event's identifier, so none is billed twice.
func (s *Store) MeterPending(ctx context.Context, m Meter, olderThan time.Duration) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, buyer_workspace_id, price_ulxc, used_at FROM market_uses
		WHERE charge = 'billed' AND metered_at IS NULL AND ran_at IS NOT NULL AND ran_at < now() - make_interval(secs => $1)
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
			return n, err
		}
		n++
	}
	return n, nil
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
		return nil, "", ErrNotRunnable
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
// [periodStart, periodEnd) — and credits each seller their share. A replay clears nothing more.
func (s *Store) ClearInvoice(ctx context.Context, buyerWorkspaceID, invoiceID string, periodStart, periodEnd, paidAt time.Time) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, seller_workspace_id, price_ulxc FROM market_uses
			WHERE buyer_workspace_id = $1 AND charge = 'billed' AND metered_at IS NOT NULL AND cleared_at IS NULL
			  AND used_at >= $2 AND used_at < $3
			ORDER BY used_at, id FOR UPDATE`, buyerWorkspaceID, periodStart, periodEnd)
		if err != nil {
			return err
		}
		type cleared struct {
			id, seller string
			ulxc       int64
		}
		var uses []cleared
		for rows.Next() {
			var c cleared
			if err := rows.Scan(&c.id, &c.seller, &c.ulxc); err != nil {
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
			// One seller's lifetime is read and extended one use at a time, whoever's invoice clears.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('market_seller:' || $1, 0))`, c.seller); err != nil {
				return err
			}
			var lifetime int64
			if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(gross_usd_micros), 0)::bigint FROM market_earnings WHERE seller_workspace_id = $1`,
				c.seller).Scan(&lifetime); err != nil {
				return err
			}
			gross := c.ulxc / ulxcPerUSDMicro
			share := SellerShare(lifetime, gross)
			if _, err := tx.Exec(ctx, `INSERT INTO market_earnings (use_id, seller_workspace_id, gross_usd_micros, share_usd_micros, invoice_id, cleared_at, payable_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, c.id, c.seller, gross, share, invoiceID, paidAt, paidAt.Add(Holdback)); err != nil {
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

// SellerShare is what a seller keeps of gross, having already earned lifetime (both µUSD): all of it up to
// US$1M lifetime, 85% of whatever lies past it.
func SellerShare(lifetime, gross int64) int64 {
	full := max(0, min(gross, FullShareUpToUSDMicros-lifetime))
	return full + (gross-full)*reducedSharePercent/100
}

// Earnings is a seller's marketplace earnings, in µUSD.
type Earnings struct {
	// Pending: uses of the seller's listings billed to buyers who have not yet paid that bill — how many,
	// and what the seller's share of them would be today. They become earnings when the invoice is paid.
	PendingUses         int64     `json:"pending_uses"`
	PendingUSDMicros    int64     `json:"pending_usd_micros"`
	PayableUSDMicros    int64     `json:"payable_usd_micros"`     // every share earned (and not yet paid out, B20.5)
	InHoldbackUSDMicros int64     `json:"in_holdback_usd_micros"` // of which still inside the 14-day holdback
	AvailableUSDMicros  int64     `json:"available_usd_micros"`   // of which past it
	LifetimeGrossMicros int64     `json:"lifetime_gross_usd_micros"`
	RefundedUSDMicros   int64     `json:"refunded_usd_micros"` // shares reversed by refunds (B20.4), already out of the totals above
	Earnings            []Earning `json:"earnings"`
}

// Earning is one cleared use's share.
type Earning struct {
	UseID          string     `json:"use_id"`
	ListingID      string     `json:"listing_id"`
	GrossUSDMicros int64      `json:"gross_usd_micros"`
	ShareUSDMicros int64      `json:"share_usd_micros"`
	InvoiceID      string     `json:"invoice_id"`
	ClearedAt      time.Time  `json:"cleared_at"`
	PayableAt      time.Time  `json:"payable_at"`
	RefundedAt     *time.Time `json:"refunded_at,omitempty"` // its share was reversed: the listing was taken down
}

// SellerEarnings reads a seller's earnings: the totals and the latest 100. An earning a refund reversed
// (B20.4) counts in none of the totals but RefundedUSDMicros.
func (s *Store) SellerEarnings(ctx context.Context, sellerWorkspaceID string, now time.Time) (Earnings, error) {
	e := Earnings{Earnings: []Earning{}}
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(e.share_usd_micros) FILTER (WHERE r.use_id IS NULL), 0)::bigint,
		       COALESCE(sum(e.share_usd_micros) FILTER (WHERE r.use_id IS NULL AND e.payable_at > $2), 0)::bigint,
		       COALESCE(sum(e.gross_usd_micros), 0)::bigint,
		       COALESCE(sum(r.reversed_share_usd_micros), 0)::bigint
		FROM market_earnings e LEFT JOIN market_refunds r ON r.use_id = e.use_id
		WHERE e.seller_workspace_id = $1`, sellerWorkspaceID, now).
		Scan(&e.PayableUSDMicros, &e.InHoldbackUSDMicros, &e.LifetimeGrossMicros, &e.RefundedUSDMicros); err != nil {
		return e, fmt.Errorf("market: earnings: %w", err)
	}
	e.AvailableUSDMicros = e.PayableUSDMicros - e.InHoldbackUSDMicros
	var pendingULXC int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(price_ulxc), 0)::bigint FROM market_uses u
		WHERE seller_workspace_id = $1 AND charge = 'billed' AND ran_at IS NOT NULL AND cleared_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = u.id)`,
		sellerWorkspaceID).Scan(&e.PendingUses, &pendingULXC); err != nil {
		return e, fmt.Errorf("market: pending earnings: %w", err)
	}
	e.PendingUSDMicros = SellerShare(e.LifetimeGrossMicros, pendingULXC/ulxcPerUSDMicro)
	rows, err := s.pool.Query(ctx, `SELECT e.use_id, COALESCE(u.listing_id, ''), e.gross_usd_micros, e.share_usd_micros, e.invoice_id, e.cleared_at, e.payable_at, r.refunded_at
		FROM market_earnings e LEFT JOIN market_uses u ON u.id = e.use_id LEFT JOIN market_refunds r ON r.use_id = e.use_id
		WHERE e.seller_workspace_id = $1 ORDER BY e.cleared_at DESC, e.use_id LIMIT 100`, sellerWorkspaceID)
	if err != nil {
		return e, fmt.Errorf("market: earnings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var x Earning
		if err := rows.Scan(&x.UseID, &x.ListingID, &x.GrossUSDMicros, &x.ShareUSDMicros, &x.InvoiceID, &x.ClearedAt, &x.PayableAt, &x.RefundedAt); err != nil {
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
	rows, err := s.pool.Query(ctx, `SELECT u.id, u.listing_id, COALESCE(l.title, ''), u.agent_id, u.price_ulxc, u.used_at, u.cleared_at, r.refunded_at
		FROM market_uses u LEFT JOIN market_listings l ON l.id = u.listing_id LEFT JOIN market_refunds r ON r.use_id = u.id
		WHERE u.buyer_workspace_id = $1 AND u.charge = 'billed' AND u.ran_at IS NOT NULL AND u.used_at >= $2 AND u.used_at < $3
		ORDER BY u.used_at, u.id`, buyerWorkspaceID, from, from.AddDate(0, 1, 0))
	if err != nil {
		return b, fmt.Errorf("market: bill: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var x BillLine
		if err := rows.Scan(&x.UseID, &x.ListingID, &x.Title, &x.AgentID, &x.PriceULXC, &x.UsedAt, &x.Cleared, &x.Refunded); err != nil {
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
