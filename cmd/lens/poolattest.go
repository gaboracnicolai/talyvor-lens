package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/econflags"
	"github.com/talyvor/lens/internal/poolsafety"
)

// poolSafetyGate returns the LIVE cross-tenant pooling decision, refreshed on a timer.
//
// ⚠ THE SEQUENCING TRAP THIS FIXES. deploy/FULL-STACK-DEPLOY.md boots the gateway (step 2b)
// BEFORE it runs `lens poolcheck` (step 6b) — necessarily, because poolcheck is invoked with
// `docker compose exec` into the running container. A gateway that decided once at boot would
// find no attestation, force pooling off, and never reconsider: the first deployment could
// never turn pooling on, and nothing would say why. Re-reading makes it self-correcting.
//
// Only the DATABASE side is re-evaluated. The model and threshold come from the process
// environment and cannot change without a restart, which is the correct asymmetry: config is
// boot-scoped, a shared mutable row is not.
func poolSafetyGate(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config) *poolsafety.Gate {
	gate := poolsafety.NewGate()
	if !cfg.CachePoolableEnabled {
		// Pooling is off in config; there is nothing to attest and no reason to poll. The
		// gate stays closed, and its reason says so rather than implying a failed check.
		return gate
	}
	db := pgxAttestationDB{pool}

	refresh := func() {
		if !gate.Refresh(ctx, db, cfg.EmbeddingModel, cfg.SemanticThreshold) {
			return // steady state: say nothing, or a 30s ticker floods the log
		}
		if gate.Attested() {
			slog.Info("POOLING ENABLED: the live embedding configuration matches the last passing poolcheck",
				slog.String("embedding_model", cfg.EmbeddingModel),
				slog.Float64("threshold", cfg.SemanticThreshold))
			return
		}
		slog.Error("POOLING FORCED OFF: cross-tenant cache pooling is enabled in config but is NOT "+
			"currently justified",
			slog.String("reason", gate.Reason()),
			slog.String("observe", "GET /v1/admin/economy/flags reports CachePoolableEnabled as "+
				"forced_off_at_runtime with this reason"))
	}

	refresh() // decide before serving the first request
	go func() {
		t := time.NewTicker(poolAttestationRefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
	return gate
}

// poolAttestationRefreshInterval bounds how long after `lens poolcheck` pooling takes to come
// on. One indexed single-row SELECT against a BOOLEAN PRIMARY KEY table per interval — the cost
// is negligible and the runbook can promise a number.
const poolAttestationRefreshInterval = 30 * time.Second

// poolFlagOverride exposes the gate's decision to the flags endpoint. Without it that endpoint
// reports CachePoolableEnabled straight off the config struct — which stays TRUE while the gate
// holds pooling off — so the one place built to observe live flag state would assert the
// opposite of the truth.
func poolFlagOverride(cfg *config.Config, gate *poolsafety.Gate) func() []econflags.Override {
	return func() []econflags.Override {
		if !cfg.CachePoolableEnabled {
			return nil // not an override: config and behaviour agree
		}
		return []econflags.Override{{
			Name:      "CachePoolableEnabled",
			Effective: gate.Attested(),
			Reason:    gate.Reason(),
		}}
	}
}

// pgxAttestationDB adapts a pgxpool to the tiny interface internal/poolsafety declares, so
// that package keeps no pgx dependency.
type pgxAttestationDB struct{ pool *pgxpool.Pool }

func (d pgxAttestationDB) QueryRow(ctx context.Context, sql string, args ...any) poolsafety.Row {
	return noRowsAsMissing{d.pool.QueryRow(ctx, sql, args...)}
}

// noRowsAsMissing translates pgx's "no rows" into poolsafety.ErrNoAttestation, so the
// poolsafety package can distinguish "never measured" from "could not read" WITHOUT importing
// pgx. The distinction is load-bearing: the first is conclusive and fails closed permanently,
// the second is a transient condition that holds the previous decision.
type noRowsAsMissing struct{ row pgx.Row }

func (r noRowsAsMissing) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return poolsafety.ErrNoAttestation
	}
	return err
}

func (d pgxAttestationDB) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := d.pool.Exec(ctx, sql, args...)
	return err
}
