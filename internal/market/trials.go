package market

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// trials.go — B32.21: FREE TRIALS IN A SANDBOX, ON TEST MONEY.
//
// A per_use offer's trial_uses (at most the store's trial max, LENS_MARKET_TRIAL_MAX) gives each buyer that many uses
// of its listing free: each is charged 'trial' (use_kind trial), never metered and never an earning, and the journal
// records the split it would have made as one test-funded entry (kind trial), so the buyer sees what it would have
// cost. Trials count per owner, not per workspace: the uses of every workspace the single-party detector ties to the
// buyer (Store.linked: a shared card fingerprint or owner key) count against the same trial_uses. A trial is
// sandboxed: a pipeline is a trial only if each step that is another seller's runs as a trial of that listing too, so
// a trial never bills anyone; the use answers trial: true; and the models it calls are the buyer's ordinary spend,
// under their own rules.

// DefaultTrialMax is the most trial uses one offer may give, until LENS_MARKET_TRIAL_MAX says otherwise — a proposal
// for Nicolai.
const DefaultTrialMax = 5

// The journal's trial accounts: what a trial use would have cost (trial:waived), and how it would have split.
const (
	AccountTrialWaived = "trial:waived"
	AccountTrialFee    = "trial:market_fee"
	JournalTrial       = "trial"
)

// TrialSeller is the account for what a seller's trial uses would have earned them. It is never a holdback or
// available account, so nothing in it is ever paid out.
func TrialSeller(workspaceID string) string { return "trial:seller:" + workspaceID }

// errTrialUsedUp: the buyer's owner had its last trial use of a listing taken by another use while this one was
// being recorded.
var errTrialUsedUp = errors.New("market: no trial use of this listing is left")

// SetTrialMax sets the most trial uses a per_use offer may give (LENS_MARKET_TRIAL_MAX).
func (s *Store) SetTrialMax(n int) { s.trialMax = &n }

func (s *Store) maxTrials() int {
	if s.trialMax == nil {
		return DefaultTrialMax
	}
	return *s.trialMax
}

type trialDB interface {
	querier
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// trialsLeft is how many trial uses of each listing buyer's owner has left: its per_use commercial offer's
// trial_uses, less the trial uses buyer and every workspace linked to it have had.
func trialsLeft(ctx context.Context, q trialDB, buyer string, listings []string) (map[string]int, error) {
	offers, err := activeOffers(ctx, q, listings...)
	if err != nil {
		return nil, err
	}
	left := map[string]int{}
	for _, id := range listings {
		given := 0
		for _, o := range offers[id] {
			if o.Kind == OfferPerUse && o.Licence == LicenceCommercial {
				given = o.TrialUses
			}
		}
		if given == 0 {
			continue
		}
		var used int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM market_uses u WHERE u.listing_id = $1 AND u.charge = 'trial'
			AND (u.buyer_workspace_id = $2 OR `+linkedSQL("$2", "u.buyer_workspace_id")+`)`, id, buyer).Scan(&used); err != nil {
			return nil, fmt.Errorf("market: trial uses: %w", err)
		}
		left[id] = max(given-used, 0)
	}
	return left, nil
}

// offerTrials makes each billed use a trial while its buyer's owner has a trial use of that listing left: u, and each
// of a pipeline's steps that is another seller's. u is a trial only if every such step is one too.
func (s *Store) offerTrials(ctx context.Context, buyer string, u *Use, steps []pipelineStep) error {
	var listings []string
	if u.Charge == ChargeBilled {
		listings = append(listings, u.ListingID)
	}
	for _, st := range steps {
		if st.UseID != "" && st.Charge == ChargeBilled {
			listings = append(listings, st.ListingID)
		}
	}
	if len(listings) == 0 {
		return nil
	}
	left, err := trialsLeft(ctx, s.pool, buyer, listings)
	if err != nil {
		return err
	}
	everyStep := true
	for i := range steps {
		st := &steps[i]
		if st.UseID == "" || st.Charge != ChargeBilled {
			continue
		}
		if left[st.ListingID] == 0 {
			everyStep = false
			continue
		}
		left[st.ListingID]--
		st.trialULXC, st.Charge, st.PriceULXC = st.PriceULXC, ChargeTrial, 0
		u.Trial = true
	}
	if u.Charge == ChargeBilled && left[u.ListingID] > 0 && everyStep {
		u.trialULXC, u.Charge, u.PriceULXC, u.Trial = u.PriceULXC, ChargeTrial, 0, true
	}
	return nil
}

// claimTrials takes the trial uses u and its steps were given, in tx: each listing's trials are held until tx commits,
// so two uses cannot both take the last one. It answers how many of u's own listing's are left after it.
func claimTrials(ctx context.Context, tx pgx.Tx, buyer string, u *Use, steps []pipelineStep) (int, error) {
	need := map[string]int{}
	if u.Charge == ChargeTrial {
		need[u.ListingID]++
	}
	for _, st := range steps {
		if st.Charge == ChargeTrial {
			need[st.ListingID]++
		}
	}
	if len(need) == 0 {
		return 0, nil
	}
	listings := make([]string, 0, len(need))
	for id := range need {
		listings = append(listings, id)
	}
	slices.Sort(listings) // one order, so two uses never wait on each other's locks
	for _, id := range listings {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('market_trial:' || $1))`, id); err != nil {
			return 0, fmt.Errorf("market: trial uses: %w", err)
		}
	}
	left, err := trialsLeft(ctx, tx, buyer, listings)
	if err != nil {
		return 0, err
	}
	for id, n := range need {
		if left[id] < n {
			return 0, errTrialUsedUp
		}
	}
	return left[u.ListingID] - need[u.ListingID], nil
}

// postTrialTx journals one trial use: the split it would have made had it been billed, on test money.
func postTrialTx(ctx context.Context, tx pgx.Tx, useID, sellerWorkspaceID string, gross int64, at time.Time) error {
	share := SellerShare(gross, takeBPS(false))
	_, err := PostJournalTx(ctx, tx, JournalTrial, useID, "test", at,
		Posting{Account: AccountTrialWaived, AmountUSDMicros: gross},
		Posting{Account: AccountTrialFee, AmountUSDMicros: -(gross - share)},
		Posting{Account: TrialSeller(sellerWorkspaceID), AmountUSDMicros: -share})
	return err
}

// useKind is what a use row charged charge is: a trial use, or a use.
func useKind(charge string) string {
	if charge == ChargeTrial {
		return "trial"
	}
	return "use"
}
