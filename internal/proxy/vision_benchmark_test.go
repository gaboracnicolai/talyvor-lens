package proxy

// vision_benchmark_test.go — the vision-OCR FIDELITY × COST benchmark (U23 OCR
// hole closer). Vision-OCR SPENDS tokens (TokensSaved=0, VisionTokensCost>0), so
// this is fidelity × cost, NOT savings × quality: does the expensive Anthropic
// OCR recover answer-relevant facts well enough to justify its cost?
//
// THREE EXECUTION MODES, kept strictly separate:
//
//  1. KEYLESS CI GUARD — TestVisionGrader_OnRecordedTranscript: validates the
//     presence grader on a fixed recorded transcript. Validates THE GRADER, NOT
//     THE MODEL. Deterministic, free, no key — runs in CI.
//     (The other keyless guard, the text-less fixture assertion, lives in
//     internal/distill/vision_fixtures_test.go.)
//  2. PAID LIVE BENCHMARK — TestVisionFidelityBenchmark_Paid: skips cleanly
//     unless BOTH ANTHROPIC_API_KEY and LENS_VISION_BENCH=1 are set. When run, it
//     OCRs each committed fixture N times via REAL Anthropic and reports
//     worst-of-N fact recovery paired with the honest VisionTokensCost. It spends
//     tokens and is non-deterministic — deliberately NOT a standing CI gate.
//  3. THROWAWAY GENERATOR — internal/distill/testdata/vision/gen_fixtures.go
//     (//go:build ignore), out of the module graph (x/image never enters go.mod).
//
// CLAIM BOUNDS (restated where any number is produced): fact recovery on CLEAN
// SYNTHETIC renders is OPTIMISTIC versus real scans (noise/skew/handwriting),
// which need real-scan fixtures. And recovery is ONE-DIRECTIONAL: it proves the
// facts were recovered, NOT that nothing spurious/hallucinated was added.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/talyvor/lens/internal/distill"
)

// ── the presence grader (re-implemented locally; U23's helper is unexported by
// design — duplicating ~6 lines is cheaper than widening an API for a test). ──

func visionNormalize(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

// visionFactsRecovered counts curated facts present (normalized substring) in the
// OCR output — reference-based, model-independent: a fact absent from the
// transcription cannot be answered by any downstream model.
func visionFactsRecovered(ocrText string, facts []string) int {
	hay := visionNormalize(ocrText)
	n := 0
	for _, f := range facts {
		if strings.Contains(hay, visionNormalize(f)) {
			n++
		}
	}
	return n
}

// TestVisionGrader_OnRecordedTranscript — KEYLESS CI GUARD. Validates the grader
// against a fixed RECORDED transcript (a representative OCR output captured once;
// NOT a live call). This validates THE GRADER, NOT THE MODEL.
func TestVisionGrader_OnRecordedTranscript(t *testing.T) {
	// A representative recorded transcript of invoice.pdf (fixed sample, no model
	// call). It contains some facts and omits others, so the grader is exercised
	// on both presence and absence.
	const transcript = "# INVOICE INV-7781\n\nBill to: Globex Corp\nAmount due: $4,200.00\n\n| Item | Qty | Unit |\n| Widget | 12 | 350.00 |"

	present := []string{"INV-7781", "Globex", "4,200.00", "350.00"}
	if got := visionFactsRecovered(transcript, present); got != len(present) {
		t.Errorf("grader missed present facts: %d/%d", got, len(present))
	}
	// The grader MUST be able to report loss — absent facts count zero (no
	// hallucinated credit). This is the property that makes the benchmark able to
	// come out low.
	absent := []string{"Delaware", "us-east-1", "99999"}
	if got := visionFactsRecovered(transcript, absent); got != 0 {
		t.Errorf("grader credited absent facts: %d, want 0", got)
	}
	// Mixed: exactly the present subset is counted.
	mixed := []string{"INV-7781", "Delaware", "Globex", "99999"}
	if got := visionFactsRecovered(transcript, mixed); got != 2 {
		t.Errorf("grader mixed count = %d, want 2", got)
	}
}

// ── the paid live benchmark ──

const (
	visionBenchKeyEnv   = "ANTHROPIC_API_KEY"       // the live key
	visionBenchOptInEnv = "LENS_VISION_BENCH"       // explicit opt-in ("1")
	visionBenchRunsEnv  = "LENS_VISION_BENCH_RUNS"  // N (default 5)
	visionBenchModelEnv = "LENS_VISION_BENCH_MODEL" // optional allow-list override
	defaultVisionRuns   = 5
	visionCallTimeout   = 90 * time.Second
)

type visionBenchFixture struct {
	File  string   `json:"file"`
	Facts []string `json:"facts"`
}

// visionFixtureDir is the committed fixture corpus, relative to this package.
const visionFixtureDir = "../distill/testdata/vision"

func loadVisionFixtures(t *testing.T) []visionBenchFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(visionFixtureDir, "facts.json"))
	if err != nil {
		t.Fatalf("read facts.json: %v", err)
	}
	var fx []visionBenchFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("parse facts.json: %v", err)
	}
	if len(fx) == 0 {
		t.Fatal("facts.json is empty")
	}
	return fx
}

// TestVisionFidelityBenchmark_Paid — PAID, env-gated, NON-deterministic. Skips
// cleanly unless ANTHROPIC_API_KEY and LENS_VISION_BENCH=1 are both set. When run,
// reports worst-of-N fact recovery × VisionTokensCost. Never a CI gate.
func TestVisionFidelityBenchmark_Paid(t *testing.T) {
	key := os.Getenv(visionBenchKeyEnv)
	if key == "" || os.Getenv(visionBenchOptInEnv) != "1" {
		t.Skipf("paid vision benchmark skipped — set %s AND %s=1 to run (spends tokens, non-deterministic; not a CI gate)",
			visionBenchKeyEnv, visionBenchOptInEnv)
	}
	runs := defaultVisionRuns
	if v := os.Getenv(visionBenchRunsEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			runs = n
		}
	}
	var allowed []string
	if m := os.Getenv(visionBenchModelEnv); m != "" {
		allowed = []string{m}
	}

	// Real-Anthropic forward injected into the PRODUCTION dispatcher seam — the
	// dispatcher (model selection, buildAnthropicVisionBody, cost extraction) is
	// the exact code the live request path runs.
	forward := func(ctx context.Context, body []byte, model string) ([]byte, int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("content-type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		return rb, resp.StatusCode, nil
	}
	d := &visionDispatcher{provider: "anthropic", allowedModels: allowed, maxTokens: visionMaxTokens, forward: forward}

	fixtures := loadVisionFixtures(t)

	var out strings.Builder
	w := tabwriter.NewWriter(&out, 0, 2, 2, ' ', 0)
	fmt.Fprintf(w, "fixture\tfacts\trecovery_min(worst)\trecovery_mean\trecovery_max\tavg_tokens_cost\terrors\n")

	corpusWorstRecovered, corpusTotal := 0, 0
	for _, fx := range fixtures {
		pdf, err := os.ReadFile(filepath.Join(visionFixtureDir, fx.File))
		if err != nil {
			t.Fatalf("%s: %v", fx.File, err)
		}
		total := len(fx.Facts)
		minR, maxR, sumR, sumCost, errCount := total, 0, 0, 0, 0
		for i := 0; i < runs; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), visionCallTimeout)
			res, err := d.DispatchVision(ctx, distill.VisionRequest{
				Bytes: pdf, MediaType: "application/pdf", Format: distill.FormatPDF, Prompt: distill.DefaultVisionPrompt,
			})
			cancel()
			if err != nil {
				// An errored OCR recovered nothing — count it honestly as a
				// worst-case 0-recovery run, and surface the error count.
				errCount++
				minR = 0
				continue
			}
			rec := visionFactsRecovered(res.Markdown, fx.Facts)
			if rec < minR {
				minR = rec
			}
			if rec > maxR {
				maxR = rec
			}
			sumR += rec
			sumCost += res.InputTokens + res.OutputTokens
		}
		meanR := float64(sumR) / float64(runs)
		avgCost := sumCost / runs
		fmt.Fprintf(w, "%s\t%d\t%d/%d\t%.1f/%d\t%d/%d\t%d\t%d\n",
			fx.File, total, minR, total, meanR, total, maxR, total, avgCost, errCount)
		corpusWorstRecovered += minR
		corpusTotal += total
	}
	w.Flush()

	worstPct := 0.0
	if corpusTotal > 0 {
		worstPct = 100 * float64(corpusWorstRecovered) / float64(corpusTotal)
	}
	t.Logf("\n=== VISION-OCR FIDELITY × COST (N=%d runs/fixture, real Anthropic) ===\n%s\n"+
		"HEADLINE (worst-of-N): recovers %d/%d = %.0f%% of answer-relevant facts, worst of %d runs.\n"+
		"BOUND: clean SYNTHETIC renders — optimistic vs real scans (need real-scan fixtures). "+
		"Recovery is one-directional (facts recovered, not 'nothing spurious added'). "+
		"Vision SPENDS tokens — the cost column is the honest VisionTokensCost, never a saving.",
		runs, out.String(), corpusWorstRecovered, corpusTotal, worstPct, runs)
}
