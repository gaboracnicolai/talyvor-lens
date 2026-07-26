package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/poolsafety"
)

// poolSafetyAttested reports whether the live embedding configuration is the one that last
// passed `lens poolcheck`. False forces cross-tenant pooling off for this process.
//
// Fail-closed in every branch: no attestation, an unreadable one, or a mismatch all return
// false. "We could not confirm this configuration was measured safe" and "it was measured
// unsafe" get the same answer, because the difference is invisible to a customer whose
// response is served to someone else.
func poolSafetyAttested(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config) bool {
	if !cfg.CachePoolableEnabled {
		return false // pooling off anyway; nothing to attest
	}
	att, err := poolsafety.Load(ctx, pgxAttestationDB{pool})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		slog.Error("POOLING FORCED OFF: no pool-safety attestation recorded for this database — "+
			"cross-tenant cache pooling is enabled in config but has never been measured safe here",
			slog.String("remedy", "run `lens poolcheck` with this deployment's env; it records the attestation on success"),
			slog.String("embedding_model", cfg.EmbeddingModel))
		return false
	case err != nil:
		slog.Error("POOLING FORCED OFF: could not read the pool-safety attestation",
			slog.String("err", err.Error()))
		return false
	}
	if ok, why := att.MatchesLive(cfg.EmbeddingModel, cfg.SemanticThreshold); !ok {
		slog.Error("POOLING FORCED OFF: the live embedding configuration is NOT the one that passed poolcheck — "+
			"the measurement that justified cross-tenant pooling no longer applies",
			slog.String("reason", why),
			slog.String("remedy", "re-run `lens poolcheck`; if it passes, pooling resumes on the next restart"))
		return false
	}
	slog.Info("pooling attested: live embedding configuration matches the last passing poolcheck",
		slog.String("embedding_model", att.EmbeddingModel),
		slog.Float64("threshold", att.Threshold),
		slog.Float64("worst_pair_score", att.WorstScore))
	return true
}

// pgxAttestationDB adapts a pgxpool to the tiny interface internal/poolsafety declares, so
// that package keeps no pgx dependency.
type pgxAttestationDB struct{ pool *pgxpool.Pool }

func (d pgxAttestationDB) QueryRow(ctx context.Context, sql string, args ...any) poolsafety.Row {
	return d.pool.QueryRow(ctx, sql, args...)
}

func (d pgxAttestationDB) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := d.pool.Exec(ctx, sql, args...)
	return err
}
