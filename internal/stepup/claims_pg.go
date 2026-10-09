package stepup

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresClaims keeps the used steps in operator_step_up_codes (0234), so a code used on one replica is refused on
// every other.
type PostgresClaims struct{ pool *pgxpool.Pool }

// NewPostgresClaims keeps the used steps in pool.
func NewPostgresClaims(pool *pgxpool.Pool) *PostgresClaims { return &PostgresClaims{pool: pool} }

// Claim records step as used by operator, and answers whether it was the first use.
func (p *PostgresClaims) Claim(ctx context.Context, step int64, operator string) (bool, error) {
	tag, err := p.pool.Exec(ctx, `INSERT INTO operator_step_up_codes (step, operator) VALUES ($1, $2) ON CONFLICT (step) DO NOTHING`,
		step, operator)
	if err != nil {
		return false, fmt.Errorf("stepup: claim: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
