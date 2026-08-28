// Package shadowmint persists what an unproven mint WOULD have paid.
//
// It is a deliberately tiny package with one job and one table. What it does NOT have is most of
// the point: no ledger type, no balance store, no transaction from the mint path.
//
// ⚠ BUT IT DOES HOLD A *pgxpool.Pool, AND THIS HEADER USED TO SAY OTHERWISE. It read "It cannot
// credit anyone because it holds nothing that could." That was the one sentence describing the
// thing that actually decides it, and it was false: cmd/lens/main.go constructs this as
// shadowmint.New(pool), and a pool for the same database is strictly MORE than the pgx.Tx that
// mining's interface guard bans. Making a shadow row real does not need a new dependency here —
// four lines in RecordShadow reach the ledger with what is already in the struct.
//
// MEASURED rather than reasoned about: a schema-valid INSERT INTO lens_token_ledger was added to
// RecordShadow and the FULL suite was run as CI runs it (real Postgres from zero, 124 migrations,
// -race, -p 1) — 106 packages ok, 0 FAIL, go vet clean. What stops it is recorder_guard_test.go,
// in this package, which reads this package's SQL. See its header for why neither guard in
// internal/mining could have.
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

// insertSQL is the ONLY statement this package runs, and it names lens_shadow_mints and nothing
// else. That is now pinned by TestShadowRecorderNamesOnlyItsOwnTable in this package.
//
// ⚠ THIS COMMENT USED TO CREDIT A TEST IN internal/mining WITH PINNING IT, AND NEITHER OF THEM
// DOES. TestShadowSink_CannotReachTheLedger asserts the ShadowSink INTERFACE takes no pgx.Tx;
// TestShadowMint_NeverTouchesTheTokenLedger reads internal/mining/shadow.go, a file that runs no
// SQL at all. The one-writer obligation was pinned on the non-writer, and the writer — this file —
// was the only package in internal/ with zero tests of its own.
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
