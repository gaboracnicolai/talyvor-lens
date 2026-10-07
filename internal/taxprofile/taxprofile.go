// Package taxprofile is a buyer's tax profile and the evidence of where a buyer is (B32.38).
//
// Talyvor is the seller of record on the marketplace, so the tax on a sale is the buyer's country's, and whether
// the buyer is a business decides whether it is charged at all. A workspace declares who it is and where — its
// legal name, address, country and, for a business, its tax id, checked with TaxPartner.ValidateTaxID when it is
// saved — and Resolve weighs that declaration against what Stripe holds: the billing country of the workspace's
// Stripe customer and the card country of its default payment method.
//
//   - Two pieces of evidence agreeing decide the country.
//   - On a conflict the declared country is used, and the profile is flagged for the operator.
//   - A workspace with no profile resolves from its Stripe billing country.
//   - With nothing at all the country is unknown, which B32.39 refuses for live consumer sales.
//
// A workspace is a business only with a valid tax id: an invalid number is stored, marked invalid, and the
// workspace stays a consumer.
package taxprofile

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/partners"
)

var (
	// ErrInvalid is a profile that cannot be saved as given.
	ErrInvalid = errors.New("taxprofile: invalid profile")
	// ErrNotFound is a workspace with no profile.
	ErrNotFound = errors.New("taxprofile: no tax profile")
)

// Taxes hands out the tax partner: *partners.Registry.
type Taxes interface {
	Tax() partners.TaxPartner
}

// StripeCountries is the Stripe read Resolve takes its evidence from: the billing country of a Stripe customer and
// the card country of its default payment method, each "" where Stripe has none. workspaceID picks the Stripe
// account the customer is in — a test workspace's is in test mode.
type StripeCountries interface {
	CustomerCountries(ctx context.Context, workspaceID, customerID string) (billingCountry, cardCountry string, err error)
}

// Profile is a workspace's buyer tax profile: a buyer_tax_profiles row.
type Profile struct {
	WorkspaceID    string     `json:"workspace_id"`
	LegalName      string     `json:"legal_name"`
	Address        string     `json:"address"`
	Country        string     `json:"country"`
	Region         string     `json:"region"`
	PostalCode     string     `json:"postal_code"`
	Business       bool       `json:"business"`
	TaxID          string     `json:"tax_id,omitempty"`            // as the tax partner normalised it
	TaxIDCheckedAt *time.Time `json:"tax_id_checked_at,omitempty"` // when the tax partner was asked
	TaxIDValid     bool       `json:"tax_id_valid"`
	TaxIDDetail    string     `json:"tax_id_detail,omitempty"` // why it is not valid
	TaxIDPartner   string     `json:"tax_id_partner,omitempty"`
	DeclaredAt     time.Time  `json:"declared_at"`
	FlaggedAt      *time.Time `json:"flagged_at,omitempty"` // when Resolve found its evidence in conflict
	FlagReason     string     `json:"flag_reason,omitempty"`
}

// Input is what a workspace declares. Business left out is true when a tax id is given.
type Input struct {
	LegalName  string `json:"legal_name"`
	Address    string `json:"address"`
	Country    string `json:"country"`
	Region     string `json:"region"`
	PostalCode string `json:"postal_code"`
	Business   *bool  `json:"business"`
	TaxID      string `json:"tax_id"`
}

// The sources of evidence of where a buyer is.
const (
	SourceDeclared      = "declared"               // the profile's country
	SourceStripeBilling = "stripe_billing_address" // the billing address of the workspace's Stripe customer
	SourceStripeCard    = "stripe_card"            // the country of the card that is its default payment method
)

// How a country was decided.
const (
	DecidedAgreed           = "agreed"               // two pieces of evidence name it
	DecidedDeclaredConflict = "declared_on_conflict" // no two agree: the declared country, and the profile is flagged
	DecidedDeclared         = "declared"             // the declaration is the only evidence
	DecidedStripeBilling    = "stripe_billing"       // no profile: the Stripe billing country
	DecidedStripeCard       = "stripe_card"          // no profile and no billing address: the card's country
	DecidedUnknown          = "unknown"              // no evidence at all
)

// Evidence is one piece of evidence of where a buyer is.
type Evidence struct {
	Source  string `json:"source"`
	Country string `json:"country"`
}

// Resolution is where a buyer is for tax, whether it is a business, and the evidence it was decided on.
type Resolution struct {
	WorkspaceID string     `json:"workspace_id"`
	Country     string     `json:"country"` // "" when unknown
	Known       bool       `json:"known"`
	Region      string     `json:"region,omitempty"`
	PostalCode  string     `json:"postal_code,omitempty"`
	Business    bool       `json:"business"`
	TaxID       string     `json:"tax_id,omitempty"` // the valid tax id a business is one by
	DecidedBy   string     `json:"decided_by"`
	Evidence    []Evidence `json:"evidence"`
	Flagged     bool       `json:"flagged"`
	FlagReason  string     `json:"flag_reason,omitempty"`
}

// Customer is the buyer as the tax partner takes it.
func (r Resolution) Customer() partners.TaxParty {
	return partners.TaxParty{ID: r.WorkspaceID, Country: r.Country, Region: r.Region, PostalCode: r.PostalCode,
		Business: r.Business, TaxID: r.TaxID, TaxIDValid: r.Business}
}

// Store keeps the profiles in Postgres and resolves a workspace's.
type Store struct {
	pool   *pgxpool.Pool
	taxes  Taxes
	stripe StripeCountries
	now    func() time.Time
}

// NewStore is the profiles in pool, their tax ids checked with taxes' partner.
func NewStore(pool *pgxpool.Pool, taxes Taxes) *Store {
	return &Store{pool: pool, taxes: taxes, now: time.Now}
}

// SetStripe is where Resolve reads the Stripe evidence. Without it a workspace has only its declaration.
func (s *Store) SetStripe(c StripeCountries) { s.stripe = c }

var countryCode = regexp.MustCompile(`^[A-Z]{2}$`)

const profileColumns = `workspace_id, legal_name, address, country, region, postal_code, business, COALESCE(tax_id, ''),
	tax_id_checked_at, tax_id_valid, tax_id_detail, tax_id_partner, declared_at, flagged_at, flag_reason`

func scanProfile(row pgx.Row) (Profile, error) {
	var p Profile
	err := row.Scan(&p.WorkspaceID, &p.LegalName, &p.Address, &p.Country, &p.Region, &p.PostalCode, &p.Business, &p.TaxID,
		&p.TaxIDCheckedAt, &p.TaxIDValid, &p.TaxIDDetail, &p.TaxIDPartner, &p.DeclaredAt, &p.FlaggedAt, &p.FlagReason)
	return p, err
}

// Get is the workspace's profile, or ErrNotFound.
func (s *Store) Get(ctx context.Context, workspaceID string) (Profile, error) {
	p, err := scanProfile(s.pool.QueryRow(ctx, `SELECT `+profileColumns+` FROM buyer_tax_profiles WHERE workspace_id = $1`, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	if err != nil {
		return Profile{}, fmt.Errorf("taxprofile: read %s: %w", workspaceID, err)
	}
	return p, nil
}

// field is a declared value, trimmed, at most max characters.
func field(name, v string, max int) (string, error) {
	v = strings.TrimSpace(v)
	if len([]rune(v)) > max {
		return "", fmt.Errorf("%w: %s is at most %d characters", ErrInvalid, name, max)
	}
	return v, nil
}

// Put saves what the workspace declares, replacing its profile. A tax id is checked with the tax partner first;
// one the partner finds not valid is saved, marked invalid. Declaring another country clears the operator's flag.
func (s *Store) Put(ctx context.Context, workspaceID string, in Input) (Profile, error) {
	p := Profile{WorkspaceID: workspaceID, Country: strings.ToUpper(strings.TrimSpace(in.Country))}
	if !countryCode.MatchString(p.Country) {
		return Profile{}, fmt.Errorf("%w: country is two letters (ISO 3166-1), such as GB or DE, not %q", ErrInvalid, in.Country)
	}
	var err error
	for _, f := range []struct {
		name string
		v    string
		max  int
		into *string
	}{
		{"legal_name", in.LegalName, 200, &p.LegalName},
		{"address", in.Address, 500, &p.Address},
		{"region", in.Region, 64, &p.Region},
		{"postal_code", in.PostalCode, 32, &p.PostalCode},
		{"tax_id", in.TaxID, 32, &p.TaxID},
	} {
		if *f.into, err = field(f.name, f.v, f.max); err != nil {
			return Profile{}, err
		}
	}
	p.Business = p.TaxID != ""
	if in.Business != nil {
		p.Business = *in.Business
	}
	if p.TaxID != "" {
		partner := s.taxes.Tax()
		res, err := partner.ValidateTaxID(ctx, p.Country, p.TaxID)
		if errors.Is(err, partners.ErrInvalid) {
			return Profile{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if err != nil {
			return Profile{}, fmt.Errorf("taxprofile: check the tax id: %w", err)
		}
		checked := s.now().UTC()
		p.TaxID, p.TaxIDValid, p.TaxIDDetail, p.TaxIDPartner, p.TaxIDCheckedAt = res.Number, res.Valid, res.Detail, partner.Name(), &checked
	}
	var taxID *string
	if p.TaxID != "" {
		taxID = &p.TaxID
	}
	saved, err := scanProfile(s.pool.QueryRow(ctx, `INSERT INTO buyer_tax_profiles (workspace_id, legal_name, address, country,
		region, postal_code, business, tax_id, tax_id_checked_at, tax_id_valid, tax_id_detail, tax_id_partner, declared_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (workspace_id) DO UPDATE SET legal_name = EXCLUDED.legal_name, address = EXCLUDED.address,
			country = EXCLUDED.country, region = EXCLUDED.region, postal_code = EXCLUDED.postal_code,
			business = EXCLUDED.business, tax_id = EXCLUDED.tax_id, tax_id_checked_at = EXCLUDED.tax_id_checked_at,
			tax_id_valid = EXCLUDED.tax_id_valid, tax_id_detail = EXCLUDED.tax_id_detail,
			tax_id_partner = EXCLUDED.tax_id_partner, declared_at = EXCLUDED.declared_at,
			flagged_at = CASE WHEN buyer_tax_profiles.country = EXCLUDED.country THEN buyer_tax_profiles.flagged_at END,
			flag_reason = CASE WHEN buyer_tax_profiles.country = EXCLUDED.country THEN buyer_tax_profiles.flag_reason ELSE '' END
		RETURNING `+profileColumns,
		workspaceID, p.LegalName, p.Address, p.Country, p.Region, p.PostalCode, p.Business, taxID, p.TaxIDCheckedAt,
		p.TaxIDValid, p.TaxIDDetail, p.TaxIDPartner, s.now().UTC()))
	if err != nil {
		return Profile{}, fmt.Errorf("taxprofile: save %s: %w", workspaceID, err)
	}
	return saved, nil
}

// stripeEvidence is the billing and card countries of the workspace's Stripe customer, "" where there is none.
func (s *Store) stripeEvidence(ctx context.Context, workspaceID string) (billing, card string, err error) {
	if s.stripe == nil {
		return "", "", nil
	}
	var customerID string
	err = s.pool.QueryRow(ctx, `SELECT stripe_customer_id FROM billing_customers WHERE workspace_id = $1`, workspaceID).Scan(&customerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("taxprofile: read %s's Stripe customer: %w", workspaceID, err)
	}
	billing, card, err = s.stripe.CustomerCountries(ctx, workspaceID, customerID)
	if err != nil {
		return "", "", fmt.Errorf("taxprofile: read %s's Stripe customer %s: %w", workspaceID, customerID, err)
	}
	return strings.ToUpper(billing), strings.ToUpper(card), nil
}

// Resolve is where the workspace is for tax and whether it is a business, with the evidence. A conflict between
// the declared country and Stripe's is flagged on the profile for the operator.
func (s *Store) Resolve(ctx context.Context, workspaceID string) (Resolution, error) {
	p, err := s.Get(ctx, workspaceID)
	hasProfile := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Resolution{}, err
	}
	billing, card, err := s.stripeEvidence(ctx, workspaceID)
	if err != nil {
		return Resolution{}, err
	}
	r := Resolution{WorkspaceID: workspaceID, Evidence: []Evidence{}}
	declared := ""
	if hasProfile {
		declared = p.Country
		r.Evidence = append(r.Evidence, Evidence{SourceDeclared, declared})
	}
	if billing != "" {
		r.Evidence = append(r.Evidence, Evidence{SourceStripeBilling, billing})
	}
	if card != "" {
		r.Evidence = append(r.Evidence, Evidence{SourceStripeCard, card})
	}
	var reason string
	r.Country, r.DecidedBy, reason = decide(declared, billing, card)
	r.Known = r.Country != ""
	if !hasProfile {
		return r, nil
	}
	if r.Country == p.Country {
		r.Region, r.PostalCode = p.Region, p.PostalCode
	}
	// The tax id was checked as the declared country's: it makes a business only where the buyer resolves to it.
	if p.Business && p.TaxIDValid && r.Country == p.Country {
		r.Business, r.TaxID = true, p.TaxID
	}
	if reason != "" && (p.FlaggedAt == nil || p.FlagReason != reason) {
		if _, err := s.pool.Exec(ctx, `UPDATE buyer_tax_profiles SET flagged_at = COALESCE(flagged_at, $3), flag_reason = $2
			WHERE workspace_id = $1`, workspaceID, reason, s.now().UTC()); err != nil {
			return Resolution{}, fmt.Errorf("taxprofile: flag %s: %w", workspaceID, err)
		}
		p.FlagReason = reason
		if p.FlaggedAt == nil {
			now := s.now().UTC()
			p.FlaggedAt = &now
		}
	}
	r.Flagged, r.FlagReason = p.FlaggedAt != nil, p.FlagReason
	return r, nil
}

// decide weighs the declared country against Stripe's billing and card countries, each "" where there is none:
// the country, how it was decided, and — when the declaration is contradicted — why the profile is flagged.
func decide(declared, billing, card string) (country, decidedBy, flag string) {
	switch {
	case billing != "" && billing == card:
		if declared != "" && declared != billing {
			return billing, DecidedAgreed, fmt.Sprintf("declared %s, but the Stripe billing address and the card both say %s", declared, billing)
		}
		return billing, DecidedAgreed, ""
	case declared != "" && (declared == billing || declared == card):
		return declared, DecidedAgreed, ""
	case declared != "" && (billing != "" || card != ""):
		var said []string
		if billing != "" {
			said = append(said, "the Stripe billing address says "+billing)
		}
		if card != "" {
			said = append(said, "the card says "+card)
		}
		return declared, DecidedDeclaredConflict, fmt.Sprintf("declared %s, but %s", declared, strings.Join(said, " and "))
	case declared != "":
		return declared, DecidedDeclared, ""
	case billing != "":
		return billing, DecidedStripeBilling, ""
	case card != "":
		return card, DecidedStripeCard, ""
	}
	return "", DecidedUnknown, ""
}

// Flagged is the profiles Resolve flagged for the operator, the oldest flag first.
func (s *Store) Flagged(ctx context.Context, limit int) ([]Profile, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `SELECT `+profileColumns+` FROM buyer_tax_profiles WHERE flagged_at IS NOT NULL
		ORDER BY flagged_at, workspace_id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("taxprofile: read the flagged profiles: %w", err)
	}
	defer rows.Close()
	out := []Profile{}
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("taxprofile: read the flagged profiles: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
