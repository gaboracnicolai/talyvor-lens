package alerts

import (
	"context"
	"fmt"
	"time"

	"github.com/talyvor/lens/internal/catalog"
)

// saving.go — B27.32: the saving Talyvor shows is measured from the workspace's own requests.
//
// Every token_events writer stamps the row with what the request would have cost at the model it asked
// for with no Talyvor cache in the path (list_cost_usd) and what it was charged (charged_usd), migration
// 0183. A workspace's saving for a month is the SUM of (list − charged) over its rows — nothing else is
// multiplied, estimated or extrapolated, so the figure on Spend is exactly what its rows add up to.

// Saving is one request's measurement, carried on the context to the token_events write so it lands on
// the SAME row as the spend (the precedent is authMethodOf: the writer reads what the request carried).
type Saving struct {
	RequestedModel string
	ListUSD        float64 // at the requested model, no Talyvor cache
	ChargedUSD     float64 // what this request was charged
}

type savingKey struct{}

// WithSaving returns ctx carrying s for the next token_events write. Pass the returned context to ONE
// writer only: a distilled request's OCR sub-call books its own row and must not inherit the saving.
func WithSaving(ctx context.Context, s Saving) context.Context {
	return context.WithValue(ctx, savingKey{}, s)
}

// savingOf is the row's three columns: ” and NULLs when the request carried no measurement.
func savingOf(ctx context.Context) (requested string, list, charged *float64) {
	s, ok := ctx.Value(savingKey{}).(Saving)
	if !ok {
		return "", nil, nil
	}
	return s.RequestedModel, &s.ListUSD, &s.ChargedUSD
}

// ListCostUSD is what a request would have cost at the model it ASKED for, sent straight to that
// provider: each class of input token at its own catalog rate, so a discount the provider's own prompt
// cache gave is in the baseline and never counted as Talyvor's saving (README, "The baseline has to be
// an already-optimised one"). It prices through the same resolver as the charge, so a request served
// at the model it asked for saves exactly zero. When the asked-for model has no exact price ("auto", or
// a model the catalog does not price) the served model is the baseline, so routing claims nothing it
// cannot price; when neither is priced the list is the charge itself — a saving of zero, never a guess.
func ListCostUSD(asked, served string, charged float64, uncachedInput, cachedInput, cacheWriteInput, output int) float64 {
	for _, m := range []string{asked, served} {
		if _, prov := catalog.ResolveRates(m, catalog.PurposeCharge); prov == catalog.ProvenanceExact {
			usd, _ := CostUSDResolved(m, catalog.PurposeCharge, uncachedInput, cachedInput, cacheWriteInput, output)
			return usd
		}
	}
	return charged
}

// MonthSaving is one workspace's measured saving for the calendar month (UTC) containing a moment.
type MonthSaving struct {
	MonthStart time.Time `json:"month_start"`
	SavedUSD   float64   `json:"saved_usd"`   // Σ (list_cost_usd − charged_usd)
	ListUSD    float64   `json:"list_usd"`    // Σ list_cost_usd
	ChargedUSD float64   `json:"charged_usd"` // Σ charged_usd
	Requests   int64     `json:"requests"`    // rows measured
	// Unmeasured counts this month's rows with no measurement (written before 0183, or an OCR sub-call):
	// left out of the sums, and said so rather than guessed.
	Unmeasured int64 `json:"unmeasured_requests"`
}

const monthSavingSQL = `
SELECT COALESCE(SUM(list_cost_usd - charged_usd) FILTER (WHERE list_cost_usd IS NOT NULL), 0),
       COALESCE(SUM(list_cost_usd) FILTER (WHERE list_cost_usd IS NOT NULL), 0),
       COALESCE(SUM(charged_usd) FILTER (WHERE list_cost_usd IS NOT NULL), 0),
       COUNT(*) FILTER (WHERE list_cost_usd IS NOT NULL),
       COUNT(*) FILTER (WHERE list_cost_usd IS NULL)
FROM token_events
WHERE workspace_id = $1 AND created_at >= $2 AND created_at < $3`

// MonthSaving sums the workspace's token_events rows for the month containing at.
func (a *AlertManager) MonthSaving(ctx context.Context, workspaceID string, at time.Time) (MonthSaving, error) {
	at = at.UTC()
	start := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	out := MonthSaving{MonthStart: start}
	if a == nil || a.pool == nil {
		return out, nil
	}
	if err := a.pool.QueryRow(ctx, monthSavingSQL, workspaceID, start, start.AddDate(0, 1, 0)).Scan(
		&out.SavedUSD, &out.ListUSD, &out.ChargedUSD, &out.Requests, &out.Unmeasured); err != nil {
		return MonthSaving{}, fmt.Errorf("alerts: month saving: %w", err)
	}
	return out, nil
}
