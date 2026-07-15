package routingbrain

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/keel"
	"github.com/talyvor/lens/internal/modelcapability"
)

// curveSource yields the H2 capability curves (modelcapability.Store satisfies it).
type curveSource interface {
	Fit(ctx context.Context) ([]modelcapability.Curve, error)
}

// driftSource yields a workspace's Keel findings (keel.Reader satisfies it). Both
// sources are DESCRIPTIVE + read-only (mint-free) — the whole reason the brain can
// consume them without a money path.
type driftSource interface {
	ListFindingsForWorkspace(ctx context.Context, workspaceID string, limit int) ([]keel.ListedFinding, error)
}

// workspaceSource enumerates the workspaces + their allow-lists to compute for.
type workspaceSource interface {
	BrainWorkspaces() []WorkspaceModels
}

const defaultFindingsLimit = 500

// Job is the OFFLINE/BATCH learn step. It reads verified outcomes (H2 curves + Keel
// drift) + the workspace allow-lists, computes a per-(workspace, difficulty)
// recommendation, and upserts it to the store. It NEVER serves a request and NEVER
// mints. Scheduled by the leader like the other analytics jobs; gated by the brain
// capability flag (off ⇒ never scheduled).
type Job struct {
	curves        curveSource
	drift         driftSource
	ws            workspaceSource
	store         *Store
	cost          func(model string) float64
	maxDifficulty int
	findingsLimit int
}

// NewJob wires the offline job. maxDifficulty defaults to modelcapability.MaxDifficulty.
func NewJob(curves curveSource, drift driftSource, ws workspaceSource, store *Store, cost func(string) float64) *Job {
	return &Job{
		curves: curves, drift: drift, ws: ws, store: store, cost: cost,
		maxDifficulty: modelcapability.MaxDifficulty, findingsLimit: defaultFindingsLimit,
	}
}

// RunOnce reads the organs, computes recommendations, and upserts them. Best-effort
// batch: a per-workspace drift read error is logged and that workspace is treated as
// having no adverse models (fail-open toward the allow-list, never toward minting —
// there is no mint). Returns the first hard error (curve read / upsert).
func (j *Job) RunOnce(ctx context.Context) error {
	if j == nil || j.store == nil {
		return nil
	}
	curves, err := j.curves.Fit(ctx)
	if err != nil {
		return err
	}
	modelCurves := make([]ModelCurve, 0, len(curves))
	for _, c := range curves {
		modelCurves = append(modelCurves, ModelCurve{Model: c.Model, Provider: c.Provider, Intercept: c.Intercept, Slope: c.Slope})
	}

	workspaces := j.ws.BrainWorkspaces()
	adverse := make(map[string]map[string]bool, len(workspaces))
	for _, w := range workspaces {
		findings, ferr := j.drift.ListFindingsForWorkspace(ctx, w.WorkspaceID, j.findingsLimit)
		if ferr != nil {
			slog.Warn("routingbrain: drift read failed; treating workspace as no-adverse",
				slog.String("workspace", w.WorkspaceID), slog.String("err", ferr.Error()))
			continue
		}
		adverse[w.WorkspaceID] = adverseModels(findings)
	}

	recs := Compute(ComputeInputs{
		MaxDifficulty: j.maxDifficulty,
		Workspaces:    workspaces,
		Curves:        modelCurves,
		Adverse:       adverse,
		Cost:          j.cost,
	})
	for _, r := range recs {
		if err := j.store.Upsert(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// adverseModels reduces a workspace's Keel findings to the set of models to AVOID:
// an IDIOSYNCRATIC drift (attributed to this tenant, not a cohort-wide shift) whose
// quality moved BELOW its cohort (deviation_sigma < 0). Common-mode drift is never
// attributed to the tenant and never bars a model.
func adverseModels(fs []keel.ListedFinding) map[string]bool {
	out := map[string]bool{}
	for _, f := range fs {
		if f.Attribution == keel.AttributionIdiosyncratic && f.DeviationSigma < 0 {
			out[modelFromUnit(f.Unit)] = true
		}
	}
	return out
}

// modelFromUnit extracts the model from a Keel unit ("provider/model" — see
// keel.Reader.CohortObservations). No slash ⇒ the whole unit is the model.
func modelFromUnit(unit string) string {
	if i := strings.Index(unit, "/"); i >= 0 {
		return unit[i+1:]
	}
	return unit
}

// StartScheduler runs RunOnce immediately then on the interval until ctx is
// cancelled. Mirrors the other analytics schedulers; the caller leader-gates it.
func (j *Job) StartScheduler(ctx context.Context, interval time.Duration) {
	if j == nil {
		return
	}
	if err := j.RunOnce(ctx); err != nil {
		slog.Warn("routingbrain: initial compute failed", slog.String("err", err.Error()))
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := j.RunOnce(ctx); err != nil {
				slog.Warn("routingbrain: compute failed", slog.String("err", err.Error()))
			}
		}
	}
}
