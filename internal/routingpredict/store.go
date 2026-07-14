// Package routingpredict is the attributable routing-PREDICTION substrate (Proof-of-Improvement piece 3,
// PR-1). A prediction is a contributor's discrete, attributed assertion — "workspace W asserts: for
// cohort C, route to model M" — the unit recon R1 found missing (routing_patterns holds anonymized
// post-serve observations, not per-contributor predictions).
//
// PR-1 is an INERT DATA SUBSTRATE: pure CRUD over routing_predictions (0070). It mints NOTHING, scores
// NOTHING, and touches NONE of the live routing/serve/Advisor path. It imports neither internal/routing
// (the serve path) nor any mint/ledger/anchor symbol — pinned by the import-guard test. Scoring against
// the held eval pool (PR-3) and minting via HeldBenchmarkAnchor (PR-4) come later.
package routingpredict

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrDuplicatePrediction is returned by SubmitPrediction when a LIVE (pending|active) prediction already
// exists for this (workspace, cohort) — the one-live-model-per-cohort anti-hedge-farm reject (mirrors
// benchprobe.ErrDuplicateItem). Retire the existing one to assert a different model.
var ErrDuplicatePrediction = errors.New("routingpredict: a live prediction already exists for this (workspace, cohort)")

// ErrSubmissionDisabled is returned when LENS_ROUTING_PREDICTION_ENABLED is off: submission is refused so
// the table stays provably empty until the capability is deliberately enabled.
var ErrSubmissionDisabled = errors.New("routingpredict: submission disabled (LENS_ROUTING_PREDICTION_ENABLED is false)")

// ErrInvalidPrediction is returned when a prediction fails cohort/field validation.
var ErrInvalidPrediction = errors.New("routingpredict: invalid prediction")

// validComplexity is the closed worktier complexity enum (worktier.ComplexityTrivial…Complex) plus ” for
// the untiered cohort. Replicated locally so routingpredict stays decoupled (it must not import the
// serve/routing/worktier path); worktier remains the source of truth for the values.
var validComplexity = map[string]bool{"": true, "trivial": true, "simple": true, "moderate": true, "complex": true}

// Prediction is one contributor-submitted routing assertion. Cohort = (FeatureCategory, InputTokenRange,
// ComplexityBucket) — the exact routing-intelligence dimensions. Provider is optional in PR-1.
type Prediction struct {
	ID               string
	WorkspaceID      string
	FeatureCategory  string
	InputTokenRange  string
	ComplexityBucket string // "" | trivial | simple | moderate | complex
	Model            string
	Provider         string
	Status           string // pending | active | retired
}

// Store is pure CRUD over routing_predictions. It holds no ledger and reaches no mint/scoring path.
type Store struct {
	pool    *pgxpool.Pool
	enabled func() bool // LENS_ROUTING_PREDICTION_ENABLED, read per call; submission refused when false/nil
}

// NewStore wires the pool and the capability flag (read per call so the gate stays live). A nil enabled
// predicate means "disabled" (safe default): SubmitPrediction refuses.
func NewStore(pool *pgxpool.Pool, enabled func() bool) *Store {
	return &Store{pool: pool, enabled: enabled}
}

// validate enforces the cohort field rules: non-empty workspace/feature/input-range/model, and a
// complexity bucket within the closed enum (or ”).
func validate(p Prediction) error {
	if p.WorkspaceID == "" {
		return fmt.Errorf("%w: workspace_id required", ErrInvalidPrediction)
	}
	if p.FeatureCategory == "" || p.InputTokenRange == "" {
		return fmt.Errorf("%w: feature_category and input_token_range required (the cohort key)", ErrInvalidPrediction)
	}
	if p.Model == "" {
		return fmt.Errorf("%w: model required", ErrInvalidPrediction)
	}
	if !validComplexity[p.ComplexityBucket] {
		return fmt.Errorf("%w: complexity_bucket %q not in {trivial,simple,moderate,complex,''}", ErrInvalidPrediction, p.ComplexityBucket)
	}
	return nil
}

// SubmitPrediction inserts a contributor's prediction as 'pending', workspace-stamped. Refused when the
// capability flag is off (ErrSubmissionDisabled). A live (pending|active) prediction for the same
// (workspace, cohort) collides on the partial-unique index → ErrDuplicatePrediction (one live model per
// cohort). Returns the new row id.
func (s *Store) SubmitPrediction(ctx context.Context, p Prediction) (string, error) {
	if s.enabled == nil || !s.enabled() {
		return "", ErrSubmissionDisabled
	}
	if err := validate(p); err != nil {
		return "", err
	}
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO routing_predictions (workspace_id, feature_category, input_token_range, complexity_bucket, model, provider, status)
		 VALUES ($1,$2,$3,$4,$5,$6,'pending') RETURNING id`,
		p.WorkspaceID, p.FeatureCategory, p.InputTokenRange, p.ComplexityBucket, p.Model, p.Provider).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation on idx_routing_predictions_live
			return "", ErrDuplicatePrediction
		}
		return "", fmt.Errorf("routingpredict: submit prediction: %w", err)
	}
	return id, nil
}

// EmitLivePrediction records a prediction from the LIVE request path (the Advisor
// picked model M for a real request and it OVERRODE the baseline) directly as
// 'active' — a real routing decision is an authoritative live assertion, no
// operator validation step. Same (workspace, cohort) dedup as SubmitPrediction
// (partial-unique) → ErrDuplicatePrediction if a live prediction already holds the
// cohort slot (one live model per cohort; the scorer/mint run offline off it).
// Gated by the same submission flag. This REPLACES the lens-routeseed CLI as the
// production emit.
func (s *Store) EmitLivePrediction(ctx context.Context, p Prediction) (string, error) {
	if s.enabled == nil || !s.enabled() {
		return "", ErrSubmissionDisabled
	}
	if err := validate(p); err != nil {
		return "", err
	}
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO routing_predictions (workspace_id, feature_category, input_token_range, complexity_bucket, model, provider, status)
		 VALUES ($1,$2,$3,$4,$5,$6,'active') RETURNING id`,
		p.WorkspaceID, p.FeatureCategory, p.InputTokenRange, p.ComplexityBucket, p.Model, p.Provider).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return "", ErrDuplicatePrediction
		}
		return "", fmt.Errorf("routingpredict: emit live prediction: %w", err)
	}
	return id, nil
}

// ValidatePrediction flips a prediction 'pending'→'active' (operator-mediated validation), making it the
// live assertion for its cohort. It stays in the dedup set (active blocks further duplicates).
func (s *Store) ValidatePrediction(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE routing_predictions SET status='active' WHERE id=$1 AND status='pending'`, id)
	if err != nil {
		return fmt.Errorf("routingpredict: validate prediction: %w", err)
	}
	return nil
}

// RetirePrediction flips a prediction to 'retired', freeing the (workspace, cohort) dedup slot so the
// workspace can assert a different model for that cohort. Retired rows are excluded from the partial unique.
func (s *Store) RetirePrediction(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE routing_predictions SET status='retired' WHERE id=$1 AND status IN ('pending','active')`, id)
	if err != nil {
		return fmt.Errorf("routingpredict: retire prediction: %w", err)
	}
	return nil
}

// GetPrediction reads one prediction by id (for tests / the future scorer). Returns nil when absent.
func (s *Store) GetPrediction(ctx context.Context, id string) (*Prediction, error) {
	var p Prediction
	err := s.pool.QueryRow(ctx,
		`SELECT id, workspace_id, feature_category, input_token_range, complexity_bucket, model, provider, status
		 FROM routing_predictions WHERE id=$1`, id).
		Scan(&p.ID, &p.WorkspaceID, &p.FeatureCategory, &p.InputTokenRange, &p.ComplexityBucket, &p.Model, &p.Provider, &p.Status)
	if err != nil {
		return nil, nil
	}
	return &p, nil
}
