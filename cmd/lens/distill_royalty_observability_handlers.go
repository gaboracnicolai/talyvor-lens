package main

// distill_royalty_observability_handlers.go — read-only, admin-gated forensic
// observability over the distill reuse-royalty (detect + margin). The distill
// mirror of pool_royalty_observability_handlers.go, with two by-design differences:
//   - NO similarity detector (distill OCR is exact-content_hash; no similarity
//     distribution) — the /detect response has no `similarity` key.
//   - NO resolver (distill has none built) — so no /resolve endpoint here.
//
// Same HARD PROPERTIES as the cache block: READ-ONLY (Query/QueryRow-only reader
// seams — no Exec/Begin reachable, no mint/burn/balance/held row touched, the distill
// adjudicate/clawback path untouched) and ADMIN-GATED but NOT economy-gated
// (forensic infra survives the LENS_ECONOMY_ENABLED kill-switch). Reuses the shared
// helpers (parseWindowParam/parseSinceQuery, selfDealingFlagDTO, marginResponse,
// poolMarginReader, thresholdsFromConfig) from the cache handlers — same package.

import (
	"context"
	"net/http"
	"time"

	"github.com/talyvor/lens/internal/poolroyalty"
)

// distillDetectReader — DistillDetectorReader satisfies it (volume + bilateral; NO similarity).
type distillDetectReader interface {
	VolumeConcentration(context.Context, time.Duration) ([]poolroyalty.DistillVolumeFlag, error)
	BilateralConcentration(context.Context, time.Duration) ([]poolroyalty.DistillSelfDealingFlag, error)
}

// distillResolveReader — DistillResolver satisfies it (volume + self_dealing; NO similarity).
type distillResolveReader interface {
	ResolveVolume(context.Context, poolroyalty.DistillVolumeFlag, time.Duration) (poolroyalty.ResolutionResult, error)
	ResolveSelfDealing(context.Context, poolroyalty.DistillSelfDealingFlag, time.Duration) (poolroyalty.ResolutionResult, error)
}

type distillVolumeFlagDTO struct {
	ContentHash                 string  `json:"content_hash"`
	ContributorWorkspace        string  `json:"contributor_workspace"`
	RequesterWorkspace          string  `json:"requester_workspace"`
	PairContentMints            int     `json:"pair_content_mints"`
	PairContentMintedUSD        float64 `json:"pair_content_minted_usd"`
	DistinctRequestersOnContent int     `json:"distinct_requesters_on_content"`
	ContentTotalMints           int     `json:"content_total_mints"`
	Flagged                     bool    `json:"flagged"`
}

// distillDetectResponse — note: NO similarity field (distill has no similarity detector).
type distillDetectResponse struct {
	WindowSeconds float64                `json:"window_seconds"`
	Volume        []distillVolumeFlagDTO `json:"volume"`
	Bilateral     []selfDealingFlagDTO   `json:"bilateral"` // reuses the cache DTO (identical shape)
}

// distillMarginDims — the distill ?by= allow-list. content_hash replaces the cache
// `layer`; a by=layer request is a clean 400 (cache-only dimension).
var distillMarginDims = map[string]bool{
	"contributor_workspace_id": true,
	"requester_workspace_id":   true,
	"content_hash":             true,
}

// newDistillRoyaltyDetectHandler — GET /v1/admin/distill-royalty/detect?window=<dur>
func newDistillRoyaltyDetectHandler(r distillDetectReader) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		window, ok := parseWindowParam(req)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "invalid window (Go duration > 0, e.g. 24h)")
			return
		}
		ctx := req.Context()
		vol, err := r.VolumeConcentration(ctx, window)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "volume detection failed")
			return
		}
		bil, err := r.BilateralConcentration(ctx, window)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "bilateral detection failed")
			return
		}
		resp := distillDetectResponse{
			WindowSeconds: window.Seconds(),
			Volume:        make([]distillVolumeFlagDTO, 0, len(vol)),
			Bilateral:     make([]selfDealingFlagDTO, 0, len(bil)),
		}
		for _, f := range vol {
			resp.Volume = append(resp.Volume, distillVolumeFlagDTO(f))
		}
		for _, f := range bil {
			resp.Bilateral = append(resp.Bilateral, selfDealingFlagDTO(f))
		}
		writeJSONOK(w, http.StatusOK, resp)
	}
}

// newDistillRoyaltyMarginHandler — GET /v1/admin/distill-royalty/margin?since=…&by=…
// (DistillMarginReader satisfies poolMarginReader; reuses the cache margin DTOs.)
func newDistillRoyaltyMarginHandler(r poolMarginReader) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		since, ok := parseSinceQuery(req)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "invalid since (RFC3339, e.g. 2026-01-02T15:04:05Z)")
			return
		}
		ctx := req.Context()
		summary, err := r.MarginSummary(ctx, since)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "margin summary failed")
			return
		}
		resp := marginResponse{Summary: marginSummaryDTO(summary)}
		if by := req.URL.Query().Get("by"); by != "" {
			if !distillMarginDims[by] {
				writeJSONErr(w, http.StatusBadRequest, "by must be one of: contributor_workspace_id, requester_workspace_id, content_hash")
				return
			}
			rows, err := r.MarginBy(ctx, by, since)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, "margin breakdown failed")
				return
			}
			resp.By = by
			resp.Breakdown = make([]marginByDTO, 0, len(rows))
			for _, row := range rows {
				resp.Breakdown = append(resp.Breakdown, marginByDTO{Key: row.Key, marginSummaryDTO: marginSummaryDTO(row.MarginSummaryRow)})
			}
		}
		writeJSONOK(w, http.StatusOK, resp)
	}
}

// newDistillRoyaltyResolveHandler — GET /v1/admin/distill-royalty/resolve?type=…&window=…
// Reconstructs the minimal distill flag from query params and returns the held
// candidates. The returned candidates[].request_id ARE the distill adjudicate endpoint's
// revoke_request_ids input (closing detect → resolve → adjudicate). Mirrors the cache
// /resolve handler; distill has only two types (volume, self_dealing) — no similarity.
func newDistillRoyaltyResolveHandler(r distillResolveReader) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		window, ok := parseWindowParam(req)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "invalid window (Go duration > 0, e.g. 24h)")
			return
		}
		q := req.URL.Query()
		ctx := req.Context()
		var res poolroyalty.ResolutionResult
		var err error
		switch q.Get("type") {
		case "volume":
			res, err = r.ResolveVolume(ctx, poolroyalty.DistillVolumeFlag{
				ContentHash:          q.Get("content_hash"),
				ContributorWorkspace: q.Get("contributor"),
			}, window)
		case "self_dealing":
			res, err = r.ResolveSelfDealing(ctx, poolroyalty.DistillSelfDealingFlag{
				ContributorWorkspace: q.Get("contributor"),
				RequesterWorkspace:   q.Get("requester"),
			}, window)
		default:
			writeJSONErr(w, http.StatusBadRequest, "type must be one of: volume, self_dealing")
			return
		}
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "resolve failed")
			return
		}
		resp := resolveResponse{Label: string(res.Label), Candidates: make([]candidateDTO, 0, len(res.Candidates))}
		for _, c := range res.Candidates {
			resp.Candidates = append(resp.Candidates, candidateDTO{
				RequestID:            c.RequestID,
				ContributorWorkspace: c.ContributorWorkspace,
				MintedAmount:         c.MintedAmount,
				Status:               c.Status,
				Similarity:           c.Similarity,
				TimeLeftSeconds:      c.TimeLeft.Seconds(),
				PastWindow:           c.PastWindow,
			})
		}
		writeJSONOK(w, http.StatusOK, resp)
	}
}
