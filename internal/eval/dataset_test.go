package eval

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/quality"
)

// fakeRecorder captures RecordSpend calls so we can assert eval spend is
// attributed through the normal cost path.
type fakeRecorder struct {
	mu    sync.Mutex
	calls []spendCall
}

type spendCall struct {
	workspaceID, feature, model, modality string
	inTok, outTok                         int
	estimated                             bool
}

func (f *fakeRecorder) RecordSpend(_ context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, spendCall{workspaceID, feature, model, modality, inputTokens, outputTokens, estimated})
	return nil
}

func TestRunCases_AttributesSpendAndAppliesTargetModel(t *testing.T) {
	srv := openAIMock(t, func(string) string { return "hello world" })
	p := newPipeline(nil, quality.New(nil), "k", "k", "k")
	p.openAIURL = srv.URL
	rec := &fakeRecorder{}
	p.SetSpendRecorder(rec)

	cases := []TestCase{
		{ID: "1", Name: "a", Model: "gpt-4o", Provider: "openai", Prompt: "q1", EvalMethod: EvalContains, ExpectedOutput: "hello", PassThreshold: 1.0},
		{ID: "2", Name: "b", Model: "gpt-4o", Provider: "openai", Prompt: "q2", EvalMethod: EvalContains, ExpectedOutput: "hello", PassThreshold: 1.0},
	}
	run, err := p.RunCases(context.Background(), "ws1", cases, Target{Model: "gpt-4o-mini", Provider: "openai"})
	if err != nil {
		t.Fatalf("RunCases: %v", err)
	}
	if run.Estimate.Cases != 2 || len(run.Results) != 2 {
		t.Fatalf("run = %+v", run)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("expected 2 spend attributions, got %d", len(rec.calls))
	}
	for _, c := range rec.calls {
		if c.model != "gpt-4o-mini" {
			t.Errorf("spend attributed to %q, want target override gpt-4o-mini", c.model)
		}
		if c.feature != "eval" || c.modality != "eval" {
			t.Errorf("spend tags = feature %q modality %q, want eval/eval", c.feature, c.modality)
		}
		if c.workspaceID != "ws1" {
			t.Errorf("workspace = %q, want ws1", c.workspaceID)
		}
	}
}

func TestRunCases_EnforcesCostCapBeforeRunning(t *testing.T) {
	// No httptest server wired — if it tried to call the model the test would
	// error, proving the cap is enforced BEFORE any spend.
	p := newPipeline(nil, quality.New(nil), "k", "k", "k")
	rec := &fakeRecorder{}
	p.SetSpendRecorder(rec)

	cases := []TestCase{
		{ID: "1", Name: "big", Model: "claude-opus-4-5", Provider: "anthropic", Prompt: "expensive", EvalMethod: EvalContains, ExpectedOutput: "x"},
	}
	_, err := p.RunCases(context.Background(), "ws1", cases, Target{MaxCostUSD: 0.0000001})
	if err == nil || !errors.Is(err, ErrCostCapExceeded) {
		t.Fatalf("want ErrCostCapExceeded, got %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("no spend should occur when cap blocks the run, got %d calls", len(rec.calls))
	}
}
