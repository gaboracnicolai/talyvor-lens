package quality

import (
	"context"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// ─── LanguageScorer ────────────────────────────────

func TestLanguageScorer_GoCodeWithFenceScoresHigh(t *testing.T) {
	ls := &LanguageScorer{Language: "go"}
	resp := "Here's the helper:\n\n```go\nfunc Add(a, b int) int {\n  return a + b\n}\n```"
	got := ls.Score(resp)
	if got < 0.8 {
		t.Errorf("score = %v, want ≥ 0.8 for fenced Go code", got)
	}
}

func TestLanguageScorer_CodeWithoutFencePenalised(t *testing.T) {
	ls := &LanguageScorer{Language: "python"}
	// Looks like code (semicolons, braces) but no fence — should
	// land in the partial-credit band.
	resp := "Just rewrite it as: x = 1; y = 2; { a: b }"
	got := ls.Score(resp)
	if got >= 0.7 || got < 0.3 {
		t.Errorf("unfenced code score = %v, want between 0.3 and 0.7", got)
	}
}

func TestLanguageScorer_ProseMultiSentenceScoresWell(t *testing.T) {
	ls := &LanguageScorer{Language: "prose"}
	resp := "The first sentence introduces the topic. The second adds detail.\n\nThe third paragraph wraps up."
	got := ls.Score(resp)
	if got < 0.8 {
		t.Errorf("multi-sentence prose score = %v, want ≥ 0.8", got)
	}
}

func TestLanguageScorer_EmptyReturnsZero(t *testing.T) {
	if got := (&LanguageScorer{Language: "go"}).Score("   "); got != 0 {
		t.Errorf("empty → %v, want 0", got)
	}
}

// ─── ScoreLength ──────────────────────────────────

func TestScoreLength_RespectsExpectedType(t *testing.T) {
	short := strings.Repeat("a", 100)
	long := strings.Repeat("a", 6000)
	if got := ScoreLength(short, "short"); got != 1 {
		t.Errorf("short response under cap = %v, want 1", got)
	}
	if got := ScoreLength(long, "short"); got >= 0.7 {
		t.Errorf("long response with type=short = %v, want < 0.7", got)
	}
	if got := ScoreLength(long, "code"); got != 1 {
		t.Errorf("6000-char code = %v, want 1 (within sweet spot)", got)
	}
}

func TestScoreLength_EmptyReturnsZero(t *testing.T) {
	if got := ScoreLength("   ", "prose"); got != 0 {
		t.Errorf("empty → %v, want 0", got)
	}
}

// ─── ScoreStructure ───────────────────────────────

func TestScoreStructure_RewardsHeadersAndCodeFences(t *testing.T) {
	resp := "## Summary\n\nHere's the plan:\n\n- step one\n- step two\n\n```go\ncode\n```"
	got := ScoreStructure(resp)
	if got < 0.9 {
		t.Errorf("rich structure = %v, want ≥ 0.9", got)
	}
}

func TestScoreStructure_BareProseStaysBaseline(t *testing.T) {
	got := ScoreStructure("Just a single line.")
	if got < 0.3 || got > 0.5 {
		t.Errorf("plain prose = %v, want ~0.4 baseline", got)
	}
}

// ─── ScoreCoherence ───────────────────────────────

func TestScoreCoherence_DeflectionGetsHalfPoint(t *testing.T) {
	resp := "I cannot help with that request."
	got := ScoreCoherence(resp)
	if got != 0.5 {
		t.Errorf("deflection = %v, want 0.5", got)
	}
}

func TestScoreCoherence_CompleteAnswerFullScore(t *testing.T) {
	resp := "The square root of 16 is 4."
	if got := ScoreCoherence(resp); got != 1 {
		t.Errorf("complete reply = %v, want 1", got)
	}
}

func TestScoreCoherence_CutOffMidSentenceZero(t *testing.T) {
	resp := strings.Repeat("this sentence keeps going and going ", 5) + "and never finishes"
	if got := ScoreCoherence(resp); got != 0 {
		t.Errorf("cut-off = %v, want 0", got)
	}
}

// ─── CompositeScorer ──────────────────────────────

func TestCompositeScorer_DefaultsToSpecWeights(t *testing.T) {
	cs := DefaultCompositeScorer()
	if cs.LengthWeight != 0.2 || cs.StructureWeight != 0.3 ||
		cs.CoherenceWeight != 0.4 || cs.LanguageWeight != 0.1 {
		t.Fatalf("default weights = %+v", cs)
	}
}

func TestCompositeScorer_HighQualityResponseScoresHigh(t *testing.T) {
	cs := DefaultCompositeScorer()
	resp := "## Solution\n\nHere is a working version:\n\n```go\nfunc Add(a, b int) int { return a + b }\n```\n\nThe key insight: integers compose under addition.\n\nThis covers the requirement."
	got := cs.ScoreWithLanguage(resp, "implement Add", "code", "go")
	if got < 0.7 {
		t.Errorf("rich code reply = %v, want ≥ 0.7", got)
	}
}

func TestCompositeScorer_DeflectionScoresLow(t *testing.T) {
	cs := DefaultCompositeScorer()
	resp := "I cannot help."
	got := cs.Score(resp, "tell me the answer", "prose")
	if got >= 0.6 {
		t.Errorf("deflection composite = %v, want < 0.6", got)
	}
}

func TestCompositeScorer_NormalisesWeights(t *testing.T) {
	// Caller passed weights that sum to 2.0 — internal
	// normalisation should still yield a result in [0,1].
	cs := &CompositeScorer{LengthWeight: 0.5, StructureWeight: 0.5, CoherenceWeight: 0.5, LanguageWeight: 0.5}
	got := cs.Score("Hello world. Good answer.", "prompt", "prose")
	if got < 0 || got > 1 {
		t.Fatalf("score = %v, want in [0,1]", got)
	}
}

// ─── ShouldAutoRetry ──────────────────────────────

func TestShouldAutoRetry_OnlyWhenEnabledAndLowAndFirstAttempt(t *testing.T) {
	if !ShouldAutoRetry(0.2, true, 0) {
		t.Error("0.2 + enabled + attempt 0 should retry")
	}
	if ShouldAutoRetry(0.2, false, 0) {
		t.Error("disabled → no retry")
	}
	if ShouldAutoRetry(0.2, true, 1) {
		t.Error("attempt 1 already used a retry — should not loop")
	}
	if ShouldAutoRetry(0.5, true, 0) {
		t.Error("0.5 above threshold → no retry")
	}
}

// ─── RecordScore + StatsForWorkspace ──────────────

func TestRecordScore_InsertsRow(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	s := newScorer(pool)

	pool.ExpectExec(`INSERT INTO quality_scores`).
		WithArgs("ws-1", 0.83, "openai", "gpt-4", "chat").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := s.RecordScore(context.Background(), "ws-1", "openai", "gpt-4", "chat", 0.83); err != nil {
		t.Fatalf("RecordScore: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestRecordScore_NilPoolIsNoop(t *testing.T) {
	s := New(nil)
	if err := s.RecordScore(context.Background(), "ws-1", "p", "m", "f", 0.5); err != nil {
		t.Fatalf("nil pool should be a noop: %v", err)
	}
}

func TestRecordScore_ClampsToZeroOne(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newScorer(pool)
	pool.ExpectExec(`INSERT INTO quality_scores`).
		WithArgs("ws-1", float64(1), "", "", "").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if err := s.RecordScore(context.Background(), "ws-1", "", "", "", 1.5); err != nil {
		t.Fatalf("RecordScore: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestStatsForWorkspace_ScansRow(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newScorer(pool)

	rows := pool.NewRows([]string{
		"sample_count", "avg_score", "p50", "p75", "p95", "low_quality",
	}).AddRow(42, 0.72, 0.75, 0.85, 0.95, 3)
	pool.ExpectQuery(`FROM quality_scores`).
		WithArgs("ws-1", pgxmock.AnyArg(), AutoRetryThreshold).
		WillReturnRows(rows)

	stats, err := s.StatsForWorkspace(context.Background(), "ws-1", 7)
	if err != nil {
		t.Fatalf("StatsForWorkspace: %v", err)
	}
	if stats.SampleCount != 42 || stats.AvgScore != 0.72 || stats.P95 != 0.95 || stats.LowQualityCount != 3 {
		t.Fatalf("stats: %+v", stats)
	}
	if stats.WindowDays != 7 {
		t.Errorf("window = %d, want 7", stats.WindowDays)
	}
}

func TestStatsForWorkspace_EmptyWorkspaceErrors(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newScorer(pool)
	if _, err := s.StatsForWorkspace(context.Background(), "  ", 30); err == nil {
		t.Fatal("expected workspace_id required error")
	}
}
