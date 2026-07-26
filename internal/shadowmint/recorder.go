// Package shadowmint persists what an unproven mint WOULD have paid.
//
// It is a deliberately tiny package with one job and one table. What it does NOT have is the
// point: no ledger, no balance store, no transaction from the mint path. It cannot credit anyone
// because it holds nothing that could. Making a shadow row real would mean adding a dependency
// here — a visible change in a file whose whole documented purpose is that it does not credit.
//
// See migrations/0108_shadow_mints.sql for why this is a separate TABLE rather than a column on
// lens_token_ledger, and internal/mining/shadow.go for where the interception happens.
package shadowmint

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbjson"
)

// insertSQL is the ONLY statement this package runs. It names lens_shadow_mints and nothing else;
// a test in internal/mining pins that the shadow path never names the ledger or balance tables.
const insertSQL = `
INSERT INTO lens_shadow_mints (workspace_id, mint_type, would_mint_micro_lens, metadata)
VALUES ($1, $2, $3, $4)`

// Recorder writes shadow rows. It holds a pool and nothing else.
type Recorder struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Recorder { return &Recorder{pool: pool} }

// RecordShadow satisfies mining.ShadowSink.
//
// ⚠ NOTE WHAT IS ABSENT FROM THIS SIGNATURE: no pgx.Tx. It cannot enlist in the mint transaction,
// so it cannot write lens_token_ledger through one, and the row it writes survives the rollback
// that the shadow interception causes. That is correct: an observation is not a financial fact
// and must not vanish with the mint it describes.
func (r *Recorder) RecordShadow(
	ctx context.Context,
	workspaceID, mintType string,
	wouldMintMicroLENS int64,
	metadata map[string]any,
) error {
	if r == nil || r.pool == nil {
		return errors.New("shadowmint: no pool")
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	// Stamp the row with what it IS, so a later reader of the table cannot mistake the number for
	// a holding even without the column comment in front of them.
	metadata["shadow"] = true
	metadata["credited"] = false

	meta, err := dbjson.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("shadowmint: marshal metadata: %w", err)
	}
	if _, err := r.pool.Exec(ctx, insertSQL, workspaceID, mintType, wouldMintMicroLENS, meta); err != nil {
		return fmt.Errorf("shadowmint: insert: %w", err)
	}
	return nil
}
