package modelwatch

// B10.5 — NEW MODELS APPEAR ON THEIR OWN.
//
// Each poll records what every provider lists (catalog_provider_lists) and every id it lists that the
// catalog cannot price (catalog_discovered_models, first seen). Every Lens instance then brings its
// in-memory catalog in line with that record (Apply, on a timer and after each poll):
//
//   - a catalog model its provider no longer lists is marked deprecated, which takes it out of the
//     chat picker (the suite offers `!deprecated && output_per_1m > 0`) while its price stays, so a
//     straggling API call is still billed correctly; it comes back if the provider lists it again;
//   - a discovered model is NOT added to the pricing catalog: it waits, unpriced, in Discovered(),
//     because a model with no price would be billed at zero (#449) — it is not free, it is not offered;
//   - a price a person confirmed (ConfirmPrice, from the provider's own pricing page, with the URL)
//     is applied to the catalog at once, and the model is offered from then on.
//
// ⚠ NOTHING HERE READS A PRICE FROM ANYWHERE BUT A PERSON. See the package comment: providers publish
// no rates in any API, so the only price that reaches the catalog is one confirmed with its source.
// A price the seed (code, pinned by published_rates_test.go) or LENS_MODEL_CATALOG_OVERRIDES already
// gives a model always wins over a confirmed discovery: a runtime row can never shadow a later seed fix.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/catalog"
)

// DefaultApplyInterval is how often every instance re-reads the record. The poll's own instance and
// the one that took a price confirmation apply immediately; the rest catch up within this.
const DefaultApplyInterval = 5 * time.Minute

// Store is the discovery record in Postgres (migrations/0133).
type Store struct{ pool *pgxpool.Pool }

// NewStore wraps pool; nil pool ⇒ nil Store (the Watcher then only alerts).
func NewStore(pool *pgxpool.Pool) *Store {
	if pool == nil {
		return nil
	}
	return &Store{pool: pool}
}

// SetStore attaches the discovery record.
func (w *Watcher) SetStore(s *Store) { w.store = s }

// Discovered is a model a provider lists that the catalog cannot price and nobody has priced yet.
type Discovered struct {
	Provider    string    `json:"provider"`
	ID          string    `json:"id"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	NeedsPrice  bool      `json:"needs_price"` // always true: the point of the list
}

type confirmedPrice struct {
	provider, id string
	in, out      float64
}

// ErrNotListed refuses a price for an id no provider lists: there is nothing to offer.
var ErrNotListed = errors.New("no provider lists this model")

// ErrAlreadyPriced refuses a price for an id the catalog already prices from the seed or an override.
var ErrAlreadyPriced = errors.New("the catalog already prices this model")

func (s *Store) recordList(ctx context.Context, provider string, ids, unknown []string, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO catalog_provider_lists (provider, model_ids, listed_at) VALUES ($1, $2, $3)
		ON CONFLICT (provider) DO UPDATE SET model_ids = EXCLUDED.model_ids, listed_at = EXCLUDED.listed_at`,
		provider, ids, now); err != nil {
		return err
	}
	if len(unknown) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO catalog_discovered_models (provider, model_id, first_seen_at)
			SELECT $1, id, $3 FROM unnest($2::text[]) AS id
			ON CONFLICT (provider, model_id) DO NOTHING`, provider, unknown, now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) lists(ctx context.Context) (map[string]map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT provider, model_ids FROM catalog_provider_lists`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var provider string
		var ids []string
		if err := rows.Scan(&provider, &ids); err != nil {
			return nil, err
		}
		set := make(map[string]bool, len(ids))
		for _, id := range ids {
			set[id] = true
		}
		out[provider] = set
	}
	return out, rows.Err()
}

func (s *Store) prices(ctx context.Context) ([]confirmedPrice, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT provider, model_id, input_per_1m, output_per_1m FROM catalog_discovered_models
		WHERE input_per_1m IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []confirmedPrice
	for rows.Next() {
		var p confirmedPrice
		if err := rows.Scan(&p.provider, &p.id, &p.in, &p.out); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// recordAndApply is the poll's half: record each provider's list, then apply. A failure is logged
// and the poll carries on — discovery must never stop the drift alert it rides on.
func (w *Watcher) recordAndApply(ctx context.Context, lists map[string][]string) {
	if w.store == nil {
		return
	}
	now := time.Now().UTC()
	for provider, ids := range lists {
		var unknown []string
		for _, id := range ids {
			if _, prov := catalog.ResolveRates(id, catalog.PurposeCharge); prov != catalog.ProvenanceExact {
				unknown = append(unknown, id)
			}
		}
		if err := w.store.recordList(ctx, provider, ids, unknown, now); err != nil {
			slog.Warn("modelwatch: could not record a provider's model list", "provider", provider, "error", err.Error())
		}
	}
	if err := w.Apply(ctx); err != nil {
		slog.Warn("modelwatch: could not apply the discovery record to the catalog", "error", err.Error())
	}
}

// Apply brings this instance's catalog in line with the record: confirmed prices in, models their
// provider no longer lists marked deprecated, relisted ones restored. Idempotent.
func (w *Watcher) Apply(ctx context.Context) error {
	if w.store == nil {
		return nil
	}
	lists, err := w.store.lists(ctx)
	if err != nil {
		return err
	}
	prices, err := w.store.prices(ctx)
	if err != nil {
		return err
	}
	w.applyMu.Lock()
	defer w.applyMu.Unlock()

	for _, p := range prices {
		// The seed or an operator override wins: only an id this Watcher put in may be re-put.
		if _, known := catalog.Get(p.id); known && !w.applied[p.id] {
			continue
		}
		catalog.Override(catalog.Model{ID: p.id, Provider: p.provider, DisplayName: p.id, InputPer1M: p.in, OutputPer1M: p.out})
		if !w.applied[p.id] {
			slog.Info("modelwatch: a confirmed price put a discovered model in the catalog", "provider", p.provider, "model", p.id)
		}
		w.applied[p.id] = true
	}

	for _, m := range catalog.All() {
		listed, polled := lists[m.Provider]
		if !polled {
			continue // a provider this deployment does not poll is never judged
		}
		inList := listed[m.ID]
		for _, a := range m.Aliases {
			inList = inList || listed[a]
		}
		before, retiredByUs := w.retired[m.ID]
		switch {
		case !inList && !retiredByUs && !m.Deprecated:
			w.retired[m.ID] = m
			m.Deprecated = true
			catalog.Override(m)
			slog.Info("modelwatch: provider no longer lists this model — marked retired; it leaves the chat picker",
				"provider", m.Provider, "model", m.ID)
		case inList && retiredByUs:
			before.InputPer1M, before.OutputPer1M = m.InputPer1M, m.OutputPer1M // keep any price applied since
			catalog.Override(before)
			delete(w.retired, m.ID)
			slog.Info("modelwatch: provider lists this model again — restored", "provider", m.Provider, "model", m.ID)
		case !inList && w.applied[m.ID] && !m.Deprecated:
			// A confirmed discovery the provider has since dropped: the price override above reset it.
			m.Deprecated = true
			catalog.Override(m)
		}
	}
	return nil
}

// ApplyLoop runs Apply on every instance, so a price confirmed on one reaches all.
func (w *Watcher) ApplyLoop(ctx context.Context, interval time.Duration) {
	if w.store == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultApplyInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := w.Apply(ctx); err != nil {
			slog.Warn("modelwatch: could not apply the discovery record to the catalog", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Discovered is every model a provider currently lists that the catalog cannot price: the list a
// person works through with ConfirmPrice. Empty (not an error) without a store.
func (w *Watcher) Discovered(ctx context.Context) ([]Discovered, error) {
	if w.store == nil {
		return []Discovered{}, nil
	}
	lists, err := w.store.lists(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := w.store.pool.Query(ctx, `SELECT provider, model_id, first_seen_at FROM catalog_discovered_models WHERE input_per_1m IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Discovered{}
	for rows.Next() {
		d := Discovered{NeedsPrice: true}
		if err := rows.Scan(&d.Provider, &d.ID, &d.FirstSeenAt); err != nil {
			return nil, err
		}
		if !lists[d.Provider][d.ID] {
			continue // the provider has since dropped it
		}
		if _, prov := catalog.ResolveRates(d.ID, catalog.PurposeCharge); prov == catalog.ProvenanceExact {
			continue // the seed or an override has priced it since
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ConfirmPrice records a person's price for a discovered model — USD per 1M tokens, read from the
// provider's own pricing page at source — and puts it in this instance's catalog at once.
func (w *Watcher) ConfirmPrice(ctx context.Context, id string, inPer1M, outPer1M float64, source string) (catalog.Model, error) {
	if w.store == nil {
		return catalog.Model{}, errors.New("model discovery has no database on this deployment")
	}
	if math.IsNaN(inPer1M) || math.IsInf(inPer1M, 0) || inPer1M <= 0 ||
		math.IsNaN(outPer1M) || math.IsInf(outPer1M, 0) || outPer1M < 0 {
		return catalog.Model{}, fmt.Errorf("input_per_1m must be above 0 and output_per_1m 0 or above (USD per 1M tokens)")
	}
	if !strings.HasPrefix(source, "https://") {
		return catalog.Model{}, fmt.Errorf("source must be the https:// URL of the provider's pricing page the price was read from")
	}
	w.applyMu.Lock()
	applied := w.applied[id]
	w.applyMu.Unlock()
	if _, known := catalog.Get(id); known && !applied {
		return catalog.Model{}, ErrAlreadyPriced
	}
	lists, err := w.store.lists(ctx)
	if err != nil {
		return catalog.Model{}, err
	}
	provider := ""
	for p, set := range lists {
		if set[id] {
			provider = p
		}
	}
	if provider == "" {
		return catalog.Model{}, ErrNotListed
	}
	if _, err := w.store.pool.Exec(ctx, `
		INSERT INTO catalog_discovered_models (provider, model_id, first_seen_at, input_per_1m, output_per_1m, price_source, priced_at)
		VALUES ($1, $2, now(), $3, $4, $5, now())
		ON CONFLICT (provider, model_id) DO UPDATE SET input_per_1m = EXCLUDED.input_per_1m,
			output_per_1m = EXCLUDED.output_per_1m, price_source = EXCLUDED.price_source, priced_at = EXCLUDED.priced_at`,
		provider, id, inPer1M, outPer1M, source); err != nil {
		return catalog.Model{}, err
	}
	if err := w.Apply(ctx); err != nil {
		return catalog.Model{}, err
	}
	m, _ := catalog.Get(id)
	return m, nil
}
