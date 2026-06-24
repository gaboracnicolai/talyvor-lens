package main

// pool_royalty_observability_handlers.go — read-only, admin-gated forensic
// observability over the Pool-B cache royalty (the detect → resolve → margin
// surfaces that already exist as tested readers but had no HTTP wiring).
//
// HARD PROPERTIES (scrutinize):
//   - READ-ONLY. Each handler holds a reader whose db seam is Query/QueryRow-only
//     (poolroyalty.{DetectorReader,Resolver,MarginReader}); there is no Exec/Begin
//     reachable, so these endpoints cannot mutate a mint/burn/balance/held row.
//     They touch NOTHING in the adjudicate/clawback path.
//   - ADMIN-GATED, NOT economy-gated. Registered on `authed` directly (NOT via the
//     econ economy-gate) but wrapped in requireAdmin → 401: forensic observability
//     must survive the LENS_ECONOMY_ENABLED kill-switch (the economy is most likely
//     OFF precisely during a security event, which is when detection is needed). The
//     MUTATION endpoint (adjudicate) stays economy-gated; these reads do not.
//
// The response DTOs (snake_case) live here so internal/poolroyalty stays untouched.

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/poolroyalty"
)

// parseFloatDefault returns the parsed float or def for an empty/invalid value
// (an absent similarity band → 0 → the resolver's unnarrowed fallback).
func parseFloatDefault(s string, def float64) float64 {
	if s == "" {
		return def
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return def
}

// thresholdsFromConfig maps the LENS_DETECT_* config knobs onto the detector's
// threshold vector (kept out of config's import graph — config holds the raw ints).
func thresholdsFromConfig(cfg *config.Config) poolroyalty.DetectorThresholds {
	return poolroyalty.DetectorThresholds{
		VolumeMinMints:      cfg.DetectVolumeMinMints,
		VolumeMaxRequesters: cfg.DetectVolumeMaxRequesters,
		BilateralMinFrac:    cfg.DetectBilateralMinFrac,
		BilateralMinMints:   cfg.DetectBilateralMinMints,
		SimilarityMinSample: cfg.DetectSimilarityMinSample,
		SimilarityMaxStddev: cfg.DetectSimilarityMaxStddev,
	}
}

// ── reader seams (the real *DetectorReader/*Resolver/*MarginReader satisfy these;
//
//	interfaces keep the handlers testable, mirroring adjudicator/adjudicateAuthenticator).
type poolDetectReader interface {
	VolumeConcentration(context.Context, time.Duration) ([]poolroyalty.VolumeFlag, error)
	BilateralConcentration(context.Context, time.Duration) ([]poolroyalty.SelfDealingFlag, error)
	SimilarityGaming(context.Context, time.Duration) ([]poolroyalty.SimilarityFlag, error)
}

type poolResolveReader interface {
	ResolveVolume(context.Context, poolroyalty.VolumeFlag, time.Duration) (poolroyalty.ResolutionResult, error)
	ResolveSelfDealing(context.Context, poolroyalty.SelfDealingFlag, time.Duration) (poolroyalty.ResolutionResult, error)
	ResolveSimilarity(context.Context, poolroyalty.SimilarityFlag, time.Duration) (poolroyalty.ResolutionResult, error)
}

type poolMarginReader interface {
	MarginSummary(context.Context, time.Time) (poolroyalty.MarginSummaryRow, error)
	MarginBy(context.Context, string, time.Time) ([]poolroyalty.MarginByRow, error)
}

// ── response DTOs (snake_case wire shape) ──────────────────────────────────────
type volumeFlagDTO struct {
	EntryID                   string  `json:"entry_id"`
	ContributorWorkspace      string  `json:"contributor_workspace"`
	RequesterWorkspace        string  `json:"requester_workspace"`
	PairEntryMints            int     `json:"pair_entry_mints"`
	PairEntryMintedUSD        float64 `json:"pair_entry_minted_usd"`
	DistinctRequestersOnEntry int     `json:"distinct_requesters_on_entry"`
	EntryTotalMints           int     `json:"entry_total_mints"`
	Flagged                   bool    `json:"flagged"`
}

type selfDealingFlagDTO struct {
	ContributorWorkspace  string  `json:"contributor_workspace"`
	RequesterWorkspace    string  `json:"requester_workspace"`
	PairMints             int     `json:"pair_mints"`
	PairMintedUSD         float64 `json:"pair_minted_usd"`
	FracOfContributorFlow float64 `json:"frac_of_contributor_flow"`
	FracOfRequesterFlow   float64 `json:"frac_of_requester_flow"`
	Flagged               bool    `json:"flagged"`
}

type similarityFlagDTO struct {
	ContributorWorkspace string  `json:"contributor_workspace"`
	EntryID              string  `json:"entry_id"`
	Hits                 int     `json:"hits"`
	DistinctPrompts      int     `json:"distinct_prompts"`
	SimMean              float64 `json:"sim_mean"`
	SimStddev            float64 `json:"sim_stddev"`
	SimMin               float64 `json:"sim_min"`
	SimMax               float64 `json:"sim_max"`
	Flagged              bool    `json:"flagged"`
}

type detectResponse struct {
	WindowSeconds float64              `json:"window_seconds"`
	Volume        []volumeFlagDTO      `json:"volume"`
	Bilateral     []selfDealingFlagDTO `json:"bilateral"`
	Similarity    []similarityFlagDTO  `json:"similarity"`
}

type candidateDTO struct {
	RequestID            string  `json:"request_id"`
	ContributorWorkspace string  `json:"contributor_workspace"`
	MintedAmount         float64 `json:"minted_amount"`
	Status               string  `json:"status"`
	Similarity           float64 `json:"similarity"`
	TimeLeftSeconds      float64 `json:"time_left_seconds"`
	PastWindow           bool    `json:"past_window"`
}

type resolveResponse struct {
	Label      string         `json:"label"`
	Candidates []candidateDTO `json:"candidates"`
}

type marginSummaryDTO struct {
	Mints          int64   `json:"mints"`
	AvoidedCOGSUSD float64 `json:"avoided_cogs_usd"`
	MintedLENS     float64 `json:"minted_lens"`
	MarginUSD      float64 `json:"margin_usd"`
}

type marginByDTO struct {
	Key string `json:"key"`
	marginSummaryDTO
}

type marginResponse struct {
	Summary   marginSummaryDTO `json:"summary"`
	By        string           `json:"by,omitempty"`
	Breakdown []marginByDTO    `json:"breakdown,omitempty"`
}

// marginDimensions is the HTTP-layer allow-list for ?by= — duplicated from the
// reader deliberately, so a bad dimension is a clean 400 (vs a 500 if the reader's
// own guard fired). The reader's guard stays as defense-in-depth.
var marginDimensions = map[string]bool{
	"contributor_workspace_id": true,
	"requester_workspace_id":   true,
	"layer":                    true,
}

// parseWindowParam: ?window=<Go duration>, default 24h; invalid/non-positive → ok=false (→400).
func parseWindowParam(req *http.Request) (time.Duration, bool) {
	v := req.URL.Query().Get("window")
	if v == "" {
		return 24 * time.Hour, true
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// parseSinceQuery: ?since=<RFC3339>, empty → zero time (all-time); invalid → ok=false (→400).
func parseSinceQuery(req *http.Request) (time.Time, bool) {
	v := req.URL.Query().Get("since")
	if v == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// newPoolRoyaltyDetectHandler — GET /v1/admin/pool-royalty/detect?window=<dur>
func newPoolRoyaltyDetectHandler(r poolDetectReader) http.HandlerFunc {
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
		sim, err := r.SimilarityGaming(ctx, window)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "similarity detection failed")
			return
		}
		resp := detectResponse{
			WindowSeconds: window.Seconds(),
			Volume:        make([]volumeFlagDTO, 0, len(vol)),
			Bilateral:     make([]selfDealingFlagDTO, 0, len(bil)),
			Similarity:    make([]similarityFlagDTO, 0, len(sim)),
		}
		for _, f := range vol {
			resp.Volume = append(resp.Volume, volumeFlagDTO(f))
		}
		for _, f := range bil {
			resp.Bilateral = append(resp.Bilateral, selfDealingFlagDTO(f))
		}
		for _, f := range sim {
			resp.Similarity = append(resp.Similarity, similarityFlagDTO(f))
		}
		writeJSONOK(w, http.StatusOK, resp)
	}
}

// newPoolRoyaltyResolveHandler — GET /v1/admin/pool-royalty/resolve?type=…&window=…
// Reconstructs the minimal flag from query params (the reader reads only
// EntryID/Contributor/Requester/SimMin/SimMax) and returns the held candidates.
func newPoolRoyaltyResolveHandler(r poolResolveReader) http.HandlerFunc {
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
			res, err = r.ResolveVolume(ctx, poolroyalty.VolumeFlag{
				EntryID:              q.Get("entry_id"),
				ContributorWorkspace: q.Get("contributor"),
				RequesterWorkspace:   q.Get("requester"),
			}, window)
		case "self_dealing":
			res, err = r.ResolveSelfDealing(ctx, poolroyalty.SelfDealingFlag{
				ContributorWorkspace: q.Get("contributor"),
				RequesterWorkspace:   q.Get("requester"),
			}, window)
		case "similarity":
			res, err = r.ResolveSimilarity(ctx, poolroyalty.SimilarityFlag{
				ContributorWorkspace: q.Get("contributor"),
				EntryID:              q.Get("entry_id"),
				SimMin:               parseFloatDefault(q.Get("sim_min"), 0),
				SimMax:               parseFloatDefault(q.Get("sim_max"), 0),
			}, window)
		default:
			writeJSONErr(w, http.StatusBadRequest, "type must be one of: volume, self_dealing, similarity")
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

// newPoolRoyaltyMarginHandler — GET /v1/admin/pool-royalty/margin?since=…&by=…
func newPoolRoyaltyMarginHandler(r poolMarginReader) http.HandlerFunc {
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
			if !marginDimensions[by] {
				writeJSONErr(w, http.StatusBadRequest, "by must be one of: contributor_workspace_id, requester_workspace_id, layer")
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
