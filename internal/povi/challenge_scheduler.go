package povi

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"math/big"
	"time"
)

// ReceiptLister is the source of challengeable receipts (the *Store satisfies
// it via ListVerifiedReceipts).
type ReceiptLister interface {
	ListVerifiedReceipts(ctx context.Context, limit int) ([]StoredReceipt, error)
}

// challengeBatchLimit bounds how many receipts a single scheduler round scans.
const challengeBatchLimit = 200

// ChallengeScheduler periodically samples verified receipts and challenges a
// `rate` fraction of them (LENS_POVI_CHALLENGE_RATE). Background / off the hot
// path. Sampling each receipt independently with probability `rate` (crypto/
// rand) keeps which receipts get challenged unpredictable.
type ChallengeScheduler struct {
	challenger *Challenger
	receipts   ReceiptLister
	rate       float64
}

// NewChallengeScheduler wires the challenger + receipt source + challenge rate.
func NewChallengeScheduler(challenger *Challenger, receipts ReceiptLister, rate float64) *ChallengeScheduler {
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	return &ChallengeScheduler{challenger: challenger, receipts: receipts, rate: rate}
}

// Rate exposes the configured challenge rate for the security-status endpoint.
func (s *ChallengeScheduler) Rate() float64 { return s.rate }

// RunOnce scans recent verified receipts and challenges each with probability
// `rate`. Returns how many challenges were issued. Already-challenged receipts
// are skipped (the double-slash guard).
func (s *ChallengeScheduler) RunOnce(ctx context.Context) int {
	if s.rate <= 0 {
		return 0
	}
	recs, err := s.receipts.ListVerifiedReceipts(ctx, challengeBatchLimit)
	if err != nil {
		slog.Error("povi: challenge scheduler list failed", slog.String("err", err.Error()))
		return 0
	}
	issued := 0
	for _, rec := range recs {
		if !coinFlip(s.rate) {
			continue
		}
		_, err := s.challenger.Challenge(ctx, rec)
		if err != nil {
			if !errors.Is(err, ErrAlreadyChallenged) {
				slog.Warn("povi: scheduled challenge failed",
					slog.String("request_id", rec.RequestID), slog.String("err", err.Error()))
			}
			continue
		}
		issued++
	}
	return issued
}

// StartScheduler is the long-lived ticker loop. Wire as
// `go scheduler.StartScheduler(ctx, interval)`.
func (s *ChallengeScheduler) StartScheduler(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Minute
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RunOnce(ctx)
		}
	}
}

// coinFlip returns true with probability p, using crypto/rand (unpredictable).
func coinFlip(p float64) bool {
	if p >= 1 {
		return true
	}
	if p <= 0 {
		return false
	}
	const scale = 1 << 30
	n, err := rand.Int(rand.Reader, big.NewInt(scale))
	if err != nil {
		return false
	}
	return float64(n.Int64())/float64(scale) < p
}
