package poolsafety

import (
	"context"
	"errors"
	"fmt"
)

// Reader and Writer are split so each caller depends only on what it uses: the gateway
// boot path reads and never writes, `lens poolcheck` writes only on success. Both are
// declared here so this package keeps no pgx dependency.
type Reader interface {
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

// Writer executes the upsert. The result is discarded — a failed write surfaces as an
// error, and a successful one always affects exactly the single row.
type Writer interface {
	Exec(ctx context.Context, sql string, args ...any) error
}

// Row mirrors pgx.Row so this package stays free of a pgx import.
type Row interface{ Scan(dest ...any) error }

// Attestation is the configuration that last passed the preflight.
type Attestation struct {
	EmbeddingModel string
	Threshold      float64
	WorstPair      string
	WorstScore     float64
}

const upsertSQL = `INSERT INTO pool_safety_attestation
  (id, embedding_model, threshold, worst_pair, worst_score, checked_at)
VALUES (true, $1, $2, $3, $4, NOW())
ON CONFLICT (id) DO UPDATE SET
  embedding_model = EXCLUDED.embedding_model,
  threshold       = EXCLUDED.threshold,
  worst_pair      = EXCLUDED.worst_pair,
  worst_score     = EXCLUDED.worst_score,
  checked_at      = NOW()`

const selectSQL = `SELECT embedding_model, threshold, worst_pair, worst_score
FROM pool_safety_attestation WHERE id = true`

// Record stores the configuration that just passed. Called by `lens poolcheck` on success —
// and ONLY on success, so the stored row always means "this was measured safe".
func Record(ctx context.Context, db Writer, a Attestation) error {
	return db.Exec(ctx, upsertSQL, a.EmbeddingModel, a.Threshold, a.WorstPair, a.WorstScore)
}

// ErrNoAttestation means the preflight has never run against this database.
var ErrNoAttestation = errors.New("poolsafety: no attestation recorded")

// Load returns the last-attested configuration.
func Load(ctx context.Context, db Reader) (Attestation, error) {
	var a Attestation
	err := db.QueryRow(ctx, selectSQL).Scan(&a.EmbeddingModel, &a.Threshold, &a.WorstPair, &a.WorstScore)
	if err != nil {
		return Attestation{}, err
	}
	return a, nil
}

// MatchesLive reports whether the attested configuration is the one now running, and if not,
// why. The reason is written into a log the operator will actually read, so a forced-off
// pooling state is never a silent one.
func (a Attestation) MatchesLive(model string, threshold float64) (bool, string) {
	if a.EmbeddingModel != model {
		return false, fmt.Sprintf("embedding model changed: attested %q, live %q", a.EmbeddingModel, model)
	}
	// The threshold is compared DIRECTIONALLY, not for equality. Raising it makes matching
	// strictly harder, so it is covered by the measurement that already passed at a lower
	// value — forcing pooling off there would be a false alarm on the conservative change,
	// which teaches operators to ignore this control. Lowering it widens what counts as a
	// match beyond anything that was measured, and that is exactly the unreviewed change
	// this binding exists to catch.
	//
	// Note what is deliberately NOT claimed: the corpus bounds the corpus, not all traffic.
	// A live threshold above the measured worst score is therefore not "proven safe" — which
	// is why the rule is "never less conservative than what was measured" rather than the
	// looser "anything above WorstScore".
	if threshold < a.Threshold {
		return false, fmt.Sprintf("similarity threshold LOWERED below what was measured: attested %.4f, live %.4f "+
			"(worst unrelated pair scored %.4f); matching is now wider than anything poolcheck examined",
			a.Threshold, threshold, a.WorstScore)
	}
	return true, ""
}
