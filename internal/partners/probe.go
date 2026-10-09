package partners

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// B37.2 — every rail is asked one read-only question every 5 minutes, so the status page shows a rail answering
// even when nothing calls it, and each rail's last answers are kept in Postgres (partner_rails), so a restart and
// every Lens process show them.

const (
	// probeRef is the fixed id and reference every probe asks with: the same each time, so a Test partner's book
	// keeps one entry for it, and a reference the partner does not know answers not found, which is an answer.
	probeRef = "talyvor-status-probe"
	// probeName is the fixed clean name the screening probe screens.
	probeName    = "Talyvor Status Probe"
	probeTimeout = 10 * time.Second
)

// probe asks every service's partner its read-only question, the configured adapter where there is one and the
// Test implementation otherwise, and answers what each said. No question sends, commits or opens anything.
func (r *Registry) probe(ctx context.Context) map[Service]error {
	r.mu.RLock()
	target := func(s Service, test any) any {
		if a, ok := r.adapters[s]; ok {
			return a
		}
		return test
	}
	account := target(ServiceAccount, r.account).(AccountPartner)
	fx := target(ServiceFX, r.fx).(FXPartner)
	broker := target(ServiceBroker, r.broker).(BrokerPartner)
	stablecoin := target(ServiceStablecoin, r.stablecoin).(StablecoinPartner)
	kyc := target(ServiceKYC, r.kyc).(KYCProvider)
	screening := target(ServiceScreening, r.screening).(ScreeningProvider)
	capital := target(ServiceCapital, r.capital).(CapitalPartner)
	insurer := target(ServiceInsurer, r.insurer).(InsurerPartner)
	agentToken := target(ServiceAgentToken, r.agentToken).(AgentTokenProvider)
	tax := target(ServiceTax, r.tax).(TaxPartner)
	r.mu.RUnlock()

	gbp := Money{100_00, "GBP"}
	asks := map[Service]func(context.Context) error{
		ServiceAccount: func(ctx context.Context) error { _, err := account.AccountDetails(ctx, probeRef); return err },
		ServiceFX: func(ctx context.Context) error {
			_, err := fx.Quote(ctx, FXQuoteRequest{ID: probeRef, Sell: gbp, Buy: "EUR"})
			return err
		},
		ServiceBroker:     func(ctx context.Context) error { _, err := broker.Quote(ctx, "TLVR", "GBP"); return err },
		ServiceStablecoin: func(ctx context.Context) error { _, err := stablecoin.Status(ctx, probeRef); return err },
		ServiceKYC:        func(ctx context.Context) error { _, err := kyc.CheckResult(ctx, probeRef); return err },
		ServiceScreening: func(ctx context.Context) error {
			_, err := screening.ScreenName(ctx, NameScreen{Name: probeName})
			return err
		},
		ServiceCapital: func(ctx context.Context) error {
			_, err := capital.Offer(ctx, CapitalRequest{ID: probeRef, Company: probeName, CompanyNumber: "00000000", Amount: gbp, TermDays: 30})
			return err
		},
		ServiceInsurer: func(ctx context.Context) error {
			_, err := insurer.Quote(ctx, CoverRequest{ID: probeRef, Insured: probeName, Cover: gbp, TermDays: 30, Risk: "status probe"})
			return err
		},
		ServiceAgentToken: func(ctx context.Context) error { _, err := agentToken.Status(ctx, probeRef); return err },
		ServiceTax: func(ctx context.Context) error {
			_, err := tax.Calculate(ctx, TaxRequest{Supplier: TaxParty{ID: SupplierTalyvor, Country: "GB"}, Customer: TaxParty{Country: "GB"},
				Lines: []TaxLine{{Ref: probeRef, TaxCode: "digital_service", AmountMicros: 1_000_000, Currency: "USD"}}, At: time.Now().UTC()})
			return err
		},
	}
	out := make(map[Service]error, len(asks))
	for _, s := range Services {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		out[s] = asks[s](pctx)
		cancel()
	}
	return out
}

// Probe asks every rail its read-only question and records each answer on the rail, as a call would.
func (r *Registry) Probe(ctx context.Context) {
	for s, err := range r.probe(ctx) {
		r.health[s].done(&err)
	}
}

// WatchRails probes every rail now and every interval after, and keeps each rail's last answers in store, from
// which this process's rails take what every other process and the last run saw. It returns when ctx ends.
func (r *Registry) WatchRails(ctx context.Context, store *RailStore, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r.Probe(ctx)
		if store != nil {
			if err := r.keepRails(ctx, store); err != nil {
				slog.Warn("partners: keeping the rails' last answers failed; the next probe tries again", slog.String("err", err.Error()))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// keepRails writes each rail's last answers to store and takes back the later of each, so the rails show what
// any process saw.
func (r *Registry) keepRails(ctx context.Context, store *RailStore) error {
	for _, s := range Services {
		h := r.health[s]
		h.mu.Lock()
		ok, failed := h.ok, h.failed
		h.mu.Unlock()
		ok, failed, err := store.keep(ctx, s, ok, failed)
		if err != nil {
			return err
		}
		h.mu.Lock()
		h.ok, h.failed = later(h.ok, ok), later(h.failed, failed)
		h.mu.Unlock()
	}
	return nil
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// RailStore is the rails' last answers in partner_rails (0237).
type RailStore struct{ pool *pgxpool.Pool }

// NewRailStore is the rails' last answers in pool.
func NewRailStore(pool *pgxpool.Pool) *RailStore { return &RailStore{pool: pool} }

// keep stores the later of the stored and the given times for s, and answers them. A zero time is none.
func (s *RailStore) keep(ctx context.Context, service Service, ok, failed time.Time) (time.Time, time.Time, error) {
	var gotOK, gotFailed *time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO partner_rails (service, last_success, last_failure) VALUES ($1, $2, $3)
		ON CONFLICT (service) DO UPDATE SET
			last_success = GREATEST(partner_rails.last_success, EXCLUDED.last_success),
			last_failure = GREATEST(partner_rails.last_failure, EXCLUDED.last_failure),
			updated_at = now()
		RETURNING last_success, last_failure`, string(service), nullTime(ok), nullTime(failed)).Scan(&gotOK, &gotFailed)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("partners: keep %s's last answers: %w", service, err)
	}
	var outOK, outFailed time.Time
	if gotOK != nil {
		outOK = gotOK.UTC()
	}
	if gotFailed != nil {
		outFailed = gotFailed.UTC()
	}
	return outOK, outFailed, nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
