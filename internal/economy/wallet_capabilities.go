package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/text/language"
)

// wallet_capabilities.go — B22.1: EVERY WALLET CAPABILITY CARRIES ITS CLASS, AND REAL MONEY OBEYS IT.
//
//	GREEN  real money now.
//	AMBER  test money; real money only once the lawyer confirms it.
//	RED    test money only, until a licence or a licensed partner exists.
//
// An AMBER or RED capability takes only test-funded money until the operator records a clearance for it
// (wallet_clearances, migration 0158: who, when, the lawyer's or partner's reference; 0194, B30.1: the licence,
// the licensed partner, the countries it covers and its expiry), and revoking the clearance stops live money
// again from the next use. A clearance past its expiry, or used from a country it does not list
// (WithUseCountry), refuses live money exactly like no clearance. A GREEN capability takes any.
//
// Money reaches a capability two ways, and both are judged:
//
//   - credits: every credit lot carries its funding on its lxc_ledger row (metadata.funding: test, live,
//     grant or synthetic), and the test-funded part of a workspace's balance is lxc_balances.test_funded_ulxc.
//     An uncleared AMBER or RED spend takes it from there, or is refused. Every other spend takes the credits
//     that are not test-funded first (writeLXCBalance), so nothing but a gated spend uses test-funded credits
//     up while others remain.
//   - a Stripe bill (a payment to another company's agent is on the company's marketplace bill, B19.15): that
//     money is live when Lens's Stripe key is live (SetLiveStripe).
//
// Going live with Stripe therefore needs no code change to stay safe: a live key makes every new purchase
// live-funded and every bill live, and neither reaches an AMBER or RED capability without a clearance.

// CapabilityClass is GREEN, AMBER or RED.
type CapabilityClass string

// The classes.
const (
	ClassGreen CapabilityClass = "GREEN"
	ClassAmber CapabilityClass = "AMBER"
	ClassRed   CapabilityClass = "RED"
)

// Capability is one thing a wallet can do.
type Capability struct {
	Key   string          `json:"capability"`
	Name  string          `json:"name"`
	Class CapabilityClass `json:"class"`
}

// The capabilities whose class code enforces today.
const (
	CapabilityPayAnotherOwner = "pay_another_owner" // AMBER: agents of different owners pay each other
	CapabilityAgentCard       = "agent_card"        // RED
)

// Capabilities is every wallet capability and its class, as Nicolai decided them on 28 Sep 2026.
var Capabilities = []Capability{
	{"spend_on_talyvor", "Spending on Talyvor", ClassGreen},
	{"buy_marketplace_listings", "Buying marketplace listings", ClassGreen},
	{"move_between_own_agents", "Moving money between one owner's own agents", ClassGreen},
	{"rules_approvals_statements_pots", "Rules, approvals, statements and pots", ClassGreen},
	{"company_credit_line", "Talyvor's credit line to companies, for Talyvor services", ClassGreen},
	{CapabilityPayAnotherOwner, "Sending and requesting money between different owners", ClassAmber},
	{"loans_between_companies", "Loans between companies", ClassAmber},
	{"escrow", "Escrow between agents", ClassAmber},
	{"cash_out", "Cashing credits out as money", ClassRed},
	{"credit_involving_a_person", "Any loan or credit involving a private user", ClassRed},
	{"interest_and_yield", "Interest or yield", ClassRed},
	{"invest_and_trade", "Investing and trading real assets", ClassRed},
	{CapabilityAgentCard, "Cards", ClassRed},

	// B30.1 — money and markets for AI agents (Nicolai, 5 Oct 2026: build everything now; real money only once
	// licensed). invest_and_trade above stays, for simulated trading.
	{CapabilityCurrencyAccounts, "Accounts in pounds, euros, dollars and USDC", ClassRed},
	{CapabilityAccountDetails, "Account details others can pay into", ClassRed},
	{CapabilityPaymentsIn, "Receiving money from outside Talyvor", ClassRed},
	{CapabilityPaymentsOut, "Paying people and companies outside Talyvor", ClassRed},
	{CapabilityPayByBank, "Topping up by a payment from your own bank", ClassAmber},
	{CapabilityFX, "Converting between currencies", ClassRed},
	{CapabilityStablecoins, "Stablecoin balances and transfers", ClassRed},
	{CapabilityX402, "Paying and being paid over HTTP 402", ClassRed},
	{CapabilityMerchantAcceptance, "Accepting payments from agents as a business", ClassRed},
	{CapabilityB2BCredit, "Credit lines and loans to companies", ClassAmber},
	{CapabilitySellerAdvances, "Advances against marketplace earnings", ClassAmber},
	{CapabilityLendingMarketplace, "Companies lending to companies through the marketplace", ClassAmber},
	{CapabilityTradeEquities, "Trading shares through a broker partner", ClassRed},
	{CapabilityTradeCrypto, "Trading crypto through a broker partner", ClassRed},
	{CapabilityTradePrediction, "Prediction-market trading", ClassRed},
	{CapabilityTreasurySweep, "Idle money in a money-market fund", ClassRed},
	{CapabilityPriceLock, "Prepaid usage at today's prices", ClassAmber},
	{CapabilityCover, "Cover for agent mistakes", ClassRed},
	{CapabilityPayoutsToPeople, "Paying people for tasks", ClassRed},
}

// The B30 capabilities. Each is asked at the point its money moves.
const (
	CapabilityCurrencyAccounts   = "currency_accounts"
	CapabilityAccountDetails     = "account_details"
	CapabilityPaymentsIn         = "payments_in"
	CapabilityPaymentsOut        = "payments_out"
	CapabilityPayByBank          = "pay_by_bank"
	CapabilityFX                 = "fx"
	CapabilityStablecoins        = "stablecoins"
	CapabilityX402               = "x402"
	CapabilityMerchantAcceptance = "merchant_acceptance"
	CapabilityB2BCredit          = "b2b_credit"
	CapabilitySellerAdvances     = "seller_advances"
	CapabilityLendingMarketplace = "lending_marketplace"
	CapabilityTradeEquities      = "trade_equities"
	CapabilityTradeCrypto        = "trade_crypto"
	CapabilityTradePrediction    = "trade_prediction"
	CapabilityTreasurySweep      = "treasury_sweep"
	CapabilityPriceLock          = "price_lock"
	CapabilityCover              = "cover"
	CapabilityPayoutsToPeople    = "payouts_to_people"
)

// CapabilityByKey finds a capability.
func CapabilityByKey(key string) (Capability, bool) {
	for _, c := range Capabilities {
		if c.Key == key {
			return c, true
		}
	}
	return Capability{}, false
}

// Funding says what paid for a credit lot, on its lxc_ledger row's metadata.
const (
	FundingTest      = "test"      // bought in Stripe test mode: test money forever
	FundingLive      = "live"      // bought with real money
	FundingGrant     = "grant"     // comped by the operator
	FundingSynthetic = "synthetic" // made inside Talyvor (LENS converted to LXC)
)

// lotFunding stamps a new credit lot's funding on its ledger metadata (a copy) and says how much of it is
// test-funded. A grant is FundingGrant. A purchase's caller says: funding test or live, or, for a lot of
// both (marketplace earnings taken as credits), test_funded_ulxc; a purchase that says nothing is live, the
// safe reading — live credits are the ones an uncleared capability refuses.
func lotFunding(ledgerType string, amount int64, metadata map[string]interface{}) (map[string]interface{}, int64) {
	out := make(map[string]interface{}, len(metadata)+1)
	for k, v := range metadata {
		out[k] = v
	}
	if ledgerType == LXCTypeGrant {
		out["funding"] = FundingGrant
		return out, 0
	}
	if part, ok := out["test_funded_ulxc"].(int64); ok {
		return out, min(max(part, 0), amount)
	}
	if out["funding"] == FundingTest {
		return out, amount
	}
	if _, ok := out["funding"]; !ok {
		out["funding"] = FundingLive
	}
	return out, 0
}

// ErrCapabilityNotCleared: live money for an AMBER or RED capability that has no clearance.
var ErrCapabilityNotCleared = errors.New("economy: this capability takes test money only until Talyvor records a clearance")

// CapabilityRefusal says which capability refused live money, and its class.
type CapabilityRefusal struct{ Capability Capability }

func (e *CapabilityRefusal) Error() string {
	return fmt.Sprintf("%s is class %s: it takes test money only until Talyvor records a clearance for it, and this would use real money",
		e.Capability.Name, e.Capability.Class)
}

// Is makes a refusal an ErrCapabilityNotCleared.
func (e *CapabilityRefusal) Is(target error) bool { return target == ErrCapabilityNotCleared }

// ClearanceTerms is what a clearance rests on and where it reaches: the lawyer's or partner's reference, the
// licence, the licensed partner, the countries live money may be used from, and when it ends.
type ClearanceTerms struct {
	Reference string    `json:"reference"`
	Licence   string    `json:"licence_reference"`
	Partner   string    `json:"partner"`
	Countries []string  `json:"countries"` // ISO 3166-1 alpha-2
	ExpiresAt time.Time `json:"expires_at"`
}

// Clearance is the operator's record that a capability may take real money.
type Clearance struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
	ClearanceTerms
}

// CapabilityStatus is a capability, its class, and whether it takes real money now.
type CapabilityStatus struct {
	Capability
	RealMoney bool       `json:"real_money"`          // GREEN, or cleared — then only in the countries the clearance lists
	Clearance *Clearance `json:"clearance,omitempty"` // the clearance in force, for an AMBER or RED one
}

type useCountryKey struct{}

// WithUseCountry says which country a use of a capability comes from (ISO 3166-1 alpha-2). A clearance lets live
// money through only for a country it lists; a use from no known country is listed by none.
func WithUseCountry(ctx context.Context, country string) context.Context {
	return context.WithValue(ctx, useCountryKey{}, strings.ToUpper(strings.TrimSpace(country)))
}

func useCountry(ctx context.Context) string {
	c, _ := ctx.Value(useCountryKey{}).(string)
	return c
}

// countryCodes checks a clearance's countries are ISO 3166-1 alpha-2 country codes, and returns them upper-case,
// each once.
func countryCodes(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, c := range in {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		r, err := language.ParseRegion(c)
		if len(c) != 2 || err != nil || !r.IsCountry() || r.String() != c {
			return nil, fmt.Errorf("economy: %q is not an ISO 3166-1 alpha-2 country code", c)
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("economy: a clearance names the countries it covers (ISO 3166-1 alpha-2, e.g. GB)")
	}
	return out, nil
}

// SetLiveStripe tells the store whether Lens's Stripe key is live, which makes a Stripe bill real money.
func (s *DualTokenStore) SetLiveStripe(live bool) { s.liveStripe = live }

// WalletCapabilities lists every capability, its class and whether it takes real money now.
func (s *DualTokenStore) WalletCapabilities(ctx context.Context) ([]CapabilityStatus, error) {
	cleared, err := clearancesInForce(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	out := make([]CapabilityStatus, 0, len(Capabilities))
	for _, c := range Capabilities {
		st := CapabilityStatus{Capability: c, RealMoney: c.Class == ClassGreen}
		if cl, ok := cleared[c.Key]; ok && c.Class != ClassGreen {
			st.RealMoney, st.Clearance = true, &cl
		}
		out = append(out, st)
	}
	return out, nil
}

// ClearCapability records that the AMBER or RED capability may take real money, on the lawyer's or partner's
// reference, under the licence and through the partner the terms name, from the countries they list, until they
// expire.
func (s *DualTokenStore) ClearCapability(ctx context.Context, key, operator string, terms ClearanceTerms) (Clearance, error) {
	countries, err := countryCodes(terms.Countries)
	if err != nil {
		return Clearance{}, err
	}
	terms.Countries = countries
	terms.Licence, terms.Partner = strings.TrimSpace(terms.Licence), strings.TrimSpace(terms.Partner)
	switch {
	case terms.Licence == "" || terms.Partner == "":
		return Clearance{}, errors.New("economy: a clearance names the licence it rests on and the licensed partner the money moves through")
	case !terms.ExpiresAt.After(time.Now()):
		return Clearance{}, errors.New("economy: a clearance needs an expiry in the future")
	}
	return s.recordClearance(ctx, key, "clear", operator, terms)
}

// RevokeClearance stops the capability taking real money, from its next use; why is recorded.
func (s *DualTokenStore) RevokeClearance(ctx context.Context, key, operator, why string) error {
	_, err := s.recordClearance(ctx, key, "revoke", operator, ClearanceTerms{Reference: why})
	return err
}

func (s *DualTokenStore) recordClearance(ctx context.Context, key, action, operator string, terms ClearanceTerms) (Clearance, error) {
	c, ok := CapabilityByKey(key)
	switch {
	case !ok:
		return Clearance{}, fmt.Errorf("economy: no wallet capability is called %q", key)
	case c.Class == ClassGreen:
		return Clearance{}, fmt.Errorf("economy: %s is GREEN and takes real money already: there is nothing to clear", c.Name)
	case strings.TrimSpace(operator) == "" || strings.TrimSpace(terms.Reference) == "":
		return Clearance{}, errors.New("economy: a clearance needs who records it and a reference (why, for a revoke)")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Clearance{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One capability's clears and revokes are ordered: the latest row is the one in force.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('wallet_clearance:' || $1, 0))`, key); err != nil {
		return Clearance{}, err
	}
	var latest string
	err = tx.QueryRow(ctx, `SELECT action FROM wallet_clearances WHERE capability = $1 ORDER BY id DESC LIMIT 1`, key).Scan(&latest)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Clearance{}, fmt.Errorf("economy: clearance: %w", err)
	}
	if action == "revoke" && latest != "clear" {
		return Clearance{}, fmt.Errorf("economy: %s has no clearance to revoke", c.Name)
	}
	var expires *time.Time
	if action == "clear" {
		expires = &terms.ExpiresAt
	}
	if terms.Countries == nil {
		terms.Countries = []string{}
	}
	cl := Clearance{By: operator, ClearanceTerms: terms}
	if err := tx.QueryRow(ctx, `INSERT INTO wallet_clearances (capability, action, operator, reference, countries, partner, licence_reference,
		expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at`, key, action, operator, terms.Reference, terms.Countries,
		terms.Partner, terms.Licence, expires).Scan(&cl.At); err != nil {
		return Clearance{}, fmt.Errorf("economy: record clearance: %w", err)
	}
	return cl, tx.Commit(ctx)
}

// ClearanceRecord is one row of the clearances' audit log.
type ClearanceRecord struct {
	Capability string    `json:"capability"`
	Action     string    `json:"action"`
	Operator   string    `json:"operator"`
	Reference  string    `json:"reference"`
	At         time.Time `json:"at"`
}

// ClearanceLog is every clear and revoke, newest first.
func (s *DualTokenStore) ClearanceLog(ctx context.Context, limit int) ([]ClearanceRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT capability, action, operator, reference, created_at FROM wallet_clearances
		ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("economy: clearances: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ClearanceRecord, error) {
		var r ClearanceRecord
		return r, row.Scan(&r.Capability, &r.Action, &r.Operator, &r.Reference, &r.At)
	})
}

// clearancesInForce is each capability's clearance whose latest row is a clear that names its countries and has
// not expired.
func clearancesInForce(ctx context.Context, q pgxDB) (map[string]Clearance, error) {
	rows, err := q.Query(ctx, `SELECT capability, operator, reference, created_at, countries, partner, licence_reference, expires_at
		FROM (SELECT DISTINCT ON (capability) * FROM wallet_clearances ORDER BY capability, id DESC) latest
		WHERE action = 'clear' AND expires_at > now() AND cardinality(countries) > 0`)
	if err != nil {
		return nil, fmt.Errorf("economy: clearances: %w", err)
	}
	defer rows.Close()
	out := map[string]Clearance{}
	for rows.Next() {
		var key string
		var c Clearance
		if err := rows.Scan(&key, &c.By, &c.Reference, &c.At, &c.Countries, &c.Partner, &c.Licence, &c.ExpiresAt); err != nil {
			return nil, err
		}
		out[key] = c
	}
	return out, rows.Err()
}

// capabilityCleared reports whether key's latest clearance row is a clear that has not expired and lists the
// country the use comes from (WithUseCountry). Anything else — a revoke, an expired clear, another country, no
// known country — is no clearance.
func capabilityCleared(ctx context.Context, q pgxDB, key string) (bool, error) {
	var cleared bool
	err := q.QueryRow(ctx, `SELECT action = 'clear' AND COALESCE(expires_at > now(), false) AND $2 = ANY(countries)
		FROM wallet_clearances WHERE capability = $1 ORDER BY id DESC LIMIT 1`, key, useCountry(ctx)).Scan(&cleared)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("economy: clearance: %w", err)
	}
	return cleared, nil
}

// spendForCapability judges a spend of amount µLXC of workspaceID's credits on capability key, in tx. A
// GREEN capability, or a cleared one, takes any credits and 0 is returned. An uncleared AMBER or RED one
// takes test-funded credits only: they are taken here and their amount returned, or the spend is refused with
// a *CapabilityRefusal when the test-funded credits do not cover it — unless the workspace is a test one, whose
// every credit is test money: it takes what test-funded credits there are, and the rest of the spend with them.
func spendForCapability(ctx context.Context, tx pgx.Tx, workspaceID, key string, amount int64) (testFunded int64, err error) {
	c, ok := CapabilityByKey(key)
	if !ok {
		return 0, fmt.Errorf("economy: no wallet capability is called %q", key)
	}
	if c.Class == ClassGreen || amount <= 0 {
		return 0, nil
	}
	if cleared, err := capabilityCleared(ctx, tx, key); err != nil || cleared {
		return 0, err
	}
	have, err := testFundedULXC(ctx, tx, workspaceID)
	if err != nil {
		return 0, err
	}
	take := amount
	if have < amount {
		// B25.3: a test workspace's credits are test money whatever funded them — its starting grant included —
		// and the wall (workspace.CheckMoneyWall) keeps them among test workspaces, so all of the spend is.
		test, err := testWorkspace(ctx, tx, workspaceID)
		if err != nil {
			return 0, err
		}
		if !test {
			return 0, &CapabilityRefusal{Capability: c}
		}
		take = have
	}
	if _, err := tx.Exec(ctx, `UPDATE lxc_balances SET test_funded_ulxc = $2 WHERE workspace_id = $1`, workspaceID, have-take); err != nil {
		return 0, fmt.Errorf("economy: take test-funded credits: %w", err)
	}
	return take, nil
}

// testWorkspace reports whether workspaceID is a test (synthetic) workspace. One with no row is real.
func testWorkspace(ctx context.Context, q pgxDB, workspaceID string) (bool, error) {
	var test bool
	if err := q.QueryRow(ctx, `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`, workspaceID).Scan(&test); err != nil {
		return false, fmt.Errorf("economy: test workspace: %w", err)
	}
	return test, nil
}

// requireBilledCapability judges money on a Stripe bill for capability key: real money when the key is live,
// which an uncleared AMBER or RED capability refuses.
func (s *DualTokenStore) requireBilledCapability(ctx context.Context, q pgxDB, key string) error {
	c, ok := CapabilityByKey(key)
	if !ok {
		return fmt.Errorf("economy: no wallet capability is called %q", key)
	}
	if c.Class == ClassGreen || !s.liveStripe {
		return nil
	}
	cleared, err := capabilityCleared(ctx, q, key)
	if err != nil || cleared {
		return err
	}
	return &CapabilityRefusal{Capability: c}
}

// testFundedULXC locks workspaceID's balance row and reads its test-funded credits: never more than the
// balance and the workspace's open holds.
func testFundedULXC(ctx context.Context, tx pgx.Tx, workspaceID string) (int64, error) {
	var v int64
	err := tx.QueryRow(ctx, `SELECT LEAST(test_funded_ulxc, GREATEST(balance + (SELECT COALESCE(sum(held_ulxc), 0) FROM lxc_reservations
		WHERE workspace_id = $1 AND status = 'held'), 0))::bigint FROM lxc_balances WHERE workspace_id = $1 FOR UPDATE`, workspaceID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("economy: test-funded credits: %w", err)
	}
	return v, nil
}

// addTestFunded returns amount µLXC of test-funded credits to workspaceID (a test purchase, or a gated spend
// given back). The caller has already credited the balance.
func addTestFunded(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64) error {
	if amount <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE lxc_balances SET test_funded_ulxc = test_funded_ulxc + $2 WHERE workspace_id = $1`,
		workspaceID, amount); err != nil {
		return fmt.Errorf("economy: add test-funded credits: %w", err)
	}
	return nil
}

// TestFundedLXC reads the test-funded part of a workspace's balance.
func (s *DualTokenStore) TestFundedLXC(ctx context.Context, workspaceID string) (int64, error) {
	var v int64
	err := s.pool.QueryRow(ctx, `SELECT test_funded_ulxc FROM lxc_balances WHERE workspace_id = $1`, workspaceID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return v, err
}
