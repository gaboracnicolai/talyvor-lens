package ab

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/quality"
)

func newTester(t *testing.T) *Tester {
	t.Helper()
	return New(nil, quality.New(nil))
}

func TestRegisterTest_RejectsSameModelForBothArms(t *testing.T) {
	tester := newTester(t)
	err := tester.RegisterTest(ABTest{
		ID: "t1", Provider: "openai",
		ModelA: "gpt-4o", ModelB: "gpt-4o",
		SamplePct: 0.5, MinSamples: 10, Active: true,
	})
	if err == nil {
		t.Fatal("expected error when ModelA == ModelB; got nil")
	}
}

func TestRegisterTest_RejectsSamplePctOutOfRange(t *testing.T) {
	tester := newTester(t)

	cases := []float64{0, -0.1, 1.5, 2}
	for _, p := range cases {
		err := tester.RegisterTest(ABTest{
			ID: "t1", Provider: "openai",
			ModelA: "gpt-4o", ModelB: "gpt-4o-mini",
			SamplePct: p, MinSamples: 10, Active: true,
		})
		if err == nil {
			t.Errorf("expected error for SamplePct=%v; got nil", p)
		}
	}
}

func TestShouldShadow_RoughlyMatchesSamplePct(t *testing.T) {
	tester := newTester(t)
	const id = "shadow-dist"
	const samplePct = 0.5
	const trials = 1000

	if err := tester.RegisterTest(ABTest{
		ID: id, Provider: "openai",
		ModelA: "gpt-4o", ModelB: "gpt-4o-mini",
		SamplePct: samplePct, MinSamples: 10, Active: true,
	}); err != nil {
		t.Fatalf("RegisterTest: %v", err)
	}

	hits := 0
	for i := 0; i < trials; i++ {
		if tester.ShouldShadow(id) {
			hits++
		}
	}

	expected := float64(trials) * samplePct
	tolerance := expected * 0.10 // 10% of expected, per spec
	if math.Abs(float64(hits)-expected) > tolerance {
		t.Errorf("ShouldShadow hits = %d, want within ±%v of %v", hits, tolerance, expected)
	}
}

func TestRunShadow_CallsUpstreamAndReturnsScoredResult(t *testing.T) {
	// Mock OpenAI upstream that echoes a known response.
	var sawModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if m, ok := body["model"].(string); ok {
			sawModel = m
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"This is a thoughtful answer with several sentences. It elaborates a bit. It ends properly."}}]}`)
	}))
	t.Cleanup(srv.Close)

	tester := newTester(t)
	tester.openAIURL = srv.URL

	if err := tester.RegisterTest(ABTest{
		ID: "t1", Provider: "openai",
		ModelA: "gpt-4o", ModelB: "gpt-4o-mini",
		SamplePct: 1.0, MinSamples: 10, Active: true,
	}); err != nil {
		t.Fatalf("RegisterTest: %v", err)
	}

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	result, err := tester.RunShadow(context.Background(), "t1", "hi", body, http.DefaultClient, "test-key")
	if err != nil {
		t.Fatalf("RunShadow: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil ShadowResult")
	}

	if sawModel != "gpt-4o-mini" {
		t.Errorf("upstream saw model = %q, want %q (RunShadow must substitute ModelB)", sawModel, "gpt-4o-mini")
	}
	if result.Model != "gpt-4o-mini" {
		t.Errorf("result.Model = %q, want %q", result.Model, "gpt-4o-mini")
	}
	if !strings.Contains(result.Response, "thoughtful answer") {
		t.Errorf("result.Response = %q, want it to contain upstream body", result.Response)
	}
	if result.Score <= 0 {
		t.Errorf("result.Score = %v, want > 0 for a well-formed response", result.Score)
	}
	if result.LatencyMs < 0 {
		t.Errorf("result.LatencyMs = %v, want non-negative", result.LatencyMs)
	}
}

func TestRecordResult_UpdatesModelStats(t *testing.T) {
	tester := newTester(t)
	const id = "t1"
	if err := tester.RegisterTest(ABTest{
		ID: id, Provider: "openai",
		ModelA: "gpt-4o", ModelB: "gpt-4o-mini",
		SamplePct: 1.0, MinSamples: 10, Active: true,
	}); err != nil {
		t.Fatalf("RegisterTest: %v", err)
	}

	_ = tester.RecordResult(context.Background(), id, "gpt-4o", ShadowResult{Score: 0.8, CostUSD: 0.10, LatencyMs: 100})
	_ = tester.RecordResult(context.Background(), id, "gpt-4o", ShadowResult{Score: 1.0, CostUSD: 0.20, LatencyMs: 200})
	_ = tester.RecordResult(context.Background(), id, "gpt-4o-mini", ShadowResult{Score: 0.9, CostUSD: 0.01, LatencyMs: 50})

	got, ok := tester.GetResults(id)
	if !ok || got == nil {
		t.Fatal("GetResults returned nil")
	}
	if got.Results.ModelA.Samples != 2 {
		t.Errorf("ModelA.Samples = %d, want 2", got.Results.ModelA.Samples)
	}
	if got.Results.ModelB.Samples != 1 {
		t.Errorf("ModelB.Samples = %d, want 1", got.Results.ModelB.Samples)
	}
	if math.Abs(got.Results.ModelA.AvgScore-0.9) > 1e-9 {
		t.Errorf("ModelA.AvgScore = %v, want 0.9", got.Results.ModelA.AvgScore)
	}
	if math.Abs(got.Results.ModelA.AvgCostUSD-0.15) > 1e-9 {
		t.Errorf("ModelA.AvgCostUSD = %v, want 0.15", got.Results.ModelA.AvgCostUSD)
	}
	if got.Results.ModelA.AvgLatencyMs != 150 {
		t.Errorf("ModelA.AvgLatencyMs = %d, want 150", got.Results.ModelA.AvgLatencyMs)
	}
}

func TestRecordResult_WinnerBWhenQualityCloseAndCostLower(t *testing.T) {
	tester := newTester(t)
	const id = "win-b"
	if err := tester.RegisterTest(ABTest{
		ID: id, Provider: "openai",
		ModelA: "gpt-4o", ModelB: "gpt-4o-mini",
		SamplePct: 1.0, MinSamples: 10, Active: true,
	}); err != nil {
		t.Fatalf("RegisterTest: %v", err)
	}

	// 10 samples each: A scores 0.90 / costs $0.10; B scores 0.87 / costs $0.01.
	// 0.87 >= 0.90 * 0.95 (=0.855) → quality within 5%
	// 0.01 < 0.10 → cheaper
	// Winner should be "B".
	for i := 0; i < 10; i++ {
		_ = tester.RecordResult(context.Background(), id, "gpt-4o", ShadowResult{Score: 0.90, CostUSD: 0.10})
		_ = tester.RecordResult(context.Background(), id, "gpt-4o-mini", ShadowResult{Score: 0.87, CostUSD: 0.01})
	}

	got, _ := tester.GetResults(id)
	if got.Results.Winner != "B" {
		t.Errorf("Winner = %q, want %q (results: %+v)", got.Results.Winner, "B", got.Results)
	}
}

func TestRecordResult_WinnerAWhenBQualitySignificantlyWorse(t *testing.T) {
	tester := newTester(t)
	const id = "win-a"
	if err := tester.RegisterTest(ABTest{
		ID: id, Provider: "openai",
		ModelA: "gpt-4o", ModelB: "gpt-4o-mini",
		SamplePct: 1.0, MinSamples: 10, Active: true,
	}); err != nil {
		t.Fatalf("RegisterTest: %v", err)
	}

	// B scores way below A's 5% band — Winner should be "A".
	for i := 0; i < 10; i++ {
		_ = tester.RecordResult(context.Background(), id, "gpt-4o", ShadowResult{Score: 0.90, CostUSD: 0.10})
		_ = tester.RecordResult(context.Background(), id, "gpt-4o-mini", ShadowResult{Score: 0.60, CostUSD: 0.01})
	}

	got, _ := tester.GetResults(id)
	if got.Results.Winner != "A" {
		t.Errorf("Winner = %q, want %q (results: %+v)", got.Results.Winner, "A", got.Results)
	}
}

func TestGetResults_NilForUnknownID(t *testing.T) {
	tester := newTester(t)
	got, ok := tester.GetResults("never-registered")
	if ok || got != nil {
		t.Errorf("GetResults returned (%v, %v); want (nil, false)", got, ok)
	}
}
