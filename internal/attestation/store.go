package attestation

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists the node_attestations record + is the single-use nonce ledger. It holds ONLY an Exec/Query
// surface — no ledger, no Begin-to-a-credit — so no mint path is reachable (import-guard test is the other
// half). Mint-free by construction; step (c) reads the verified rows.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store wraps a pool. nil-pool ⇒ no-op issue / never-consumes (safe for flag-off).
type Store struct {
	db execer
}

func NewStore(pool *pgxpool.Pool) *Store {
	if pool == nil {
		return &Store{}
	}
	return &Store{db: pool}
}

// IssueNonce records a fresh single-use challenge as a 'pending' row BOUND to nodeID. The nonce PK makes a
// duplicate issue fail; node_id is the relay/cross-node binding checked at Consume.
func (s *Store) IssueNonce(ctx context.Context, nonce int64, nodeID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO node_attestations (nonce, node_id, attestation_status, attested_at)
		 VALUES ($1, $2, 'pending', now())`, nonce, nodeID)
	if err != nil {
		return fmt.Errorf("attestation: issue nonce: %w", err)
	}
	return nil
}

// ConsumeResult is what a verify produced (or the failure marker).
type ConsumeResult struct {
	Status    string // 'verified' | 'failed'
	GPUClass  string
	CCMode    bool
	EATDigest string
	KeyBound  bool
	ValidFor  time.Duration // attestation validity from now (verified only)
}

// consumeSQL atomically consumes the pending nonce for THIS node, within the challenge window, and records
// the result. RowsAffected()==0 ⇒ the nonce was already consumed (replay), issued to a DIFFERENT node
// (relay/cross-node), or the window elapsed — all REJECT. Mirrors the pool-royalty finalize CAS.
const consumeSQL = `UPDATE node_attestations
    SET attestation_status = $3, attested_gpu_class = $4, cc_mode = $5, eat_digest = $6, key_bound = $7,
        expires_at = CASE WHEN $3 = 'verified' THEN now() + ($8::bigint * interval '1 microsecond') ELSE expires_at END
  WHERE nonce = $1 AND node_id = $2 AND attestation_status = 'pending'
    AND attested_at > now() - ($9::bigint * interval '1 microsecond')`

// Consume applies the CAS. Returns (true, nil) if the nonce was consumed by THIS call (single-use won),
// (false, nil) if it was already consumed / wrong node / window elapsed (REJECT). challengeWindow bounds how
// long a pending nonce stays answerable.
func (s *Store) Consume(ctx context.Context, nonce int64, nodeID string, r ConsumeResult, challengeWindow time.Duration) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	var gpuClass, digest any
	if r.Status == "verified" {
		gpuClass, digest = r.GPUClass, r.EATDigest
	}
	tag, err := s.db.Exec(ctx, consumeSQL,
		nonce, nodeID, r.Status, gpuClass, r.CCMode, digest, r.KeyBound,
		r.ValidFor.Microseconds(), challengeWindow.Microseconds())
	if err != nil {
		return false, fmt.Errorf("attestation: consume: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
