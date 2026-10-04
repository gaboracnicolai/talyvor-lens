package kompress

// measured_test.go — B27.35's DONE, against the real weights: on real request payloads the model
// shrinks text phase 1 refuses by a measured percentage, and the answers in the known-answer
// scenarios survive it. CI runs this in the "Tare phase 2a model" step with TARE_MODEL_REQUIRED=1,
// so a missing model fails rather than skips.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/tare"
)

// chatBody wraps content as the newest message of a real chat request, behind a system prompt and a
// turn of history, so it goes through tare.PrefixStable exactly as the serve path sends it.
func chatBody(t *testing.T, content string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model":  "claude-haiku-4-5",
		"system": "You are a careful engineering assistant.",
		"messages": []map[string]any{
			{"role": "user", "content": "I am going to paste some context."},
			{"role": "assistant", "content": "Go ahead."},
			{"role": "user", "content": content},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newestContent(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("reduced body is not valid JSON: %v", err)
	}
	return req.Messages[len(req.Messages)-1].Content
}

// docsProse is the population: every paragraph of this repo's docs/*.md with at least 40 words and
// no code fence — real technical prose written for and by the people and agents that use Lens.
func docsProse(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../../../docs/*.md")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range strings.Split(string(b), "\n\n") {
			if len(strings.Fields(p)) >= 40 && !strings.Contains(p, "```") {
				out = append(out, p)
			}
		}
	}
	return out
}

func TestPhase2a_ShrinksRealPayloadsPhase1Refuses(t *testing.T) {
	dir := modelDir(t)
	model := tare.NewPrefixStable(NewCompressor(dir).Reduction(nil), tare.KindProse)
	ctx := context.Background()

	const want = 60
	var n, reduced, bytesIn, bytesOut int
	for _, p := range docsProse(t) {
		if n == want {
			break
		}
		// Only where phase 1 refuses: run every phase-1 reducer the way the serve path does and
		// skip anything one of them shrank.
		if pre, err := tare.Preview(ctx, []byte(p), ""); err != nil || !pre.Refused {
			continue
		}
		body := chatBody(t, p)
		out, _, _, err := model.Reduce(ctx, body, tare.KindProse)
		if err != nil {
			t.Fatal(err)
		}
		n++
		got := newestContent(t, out)
		if at := strings.LastIndex(string(body), `"content":`); !strings.HasPrefix(string(out), string(body[:at])) {
			t.Fatalf("payload %d: a byte before the newest message's content changed", n)
		}
		if len(got) < len(p) {
			reduced++
		}
		bytesIn += len(p)
		bytesOut += len(got)
	}
	if n < want {
		t.Fatalf("population: %d payloads phase 1 refuses, want %d — the measurement would be over too few", n, want)
	}
	pct := 100 * float64(bytesIn-bytesOut) / float64(bytesIn)
	t.Logf("MEASURED: %d real payloads phase 1 refused; the model reduced %d of them; %d → %d bytes of newest-message prose, %.1f%% smaller", n, reduced, bytesIn, bytesOut, pct)
	if pct < 10 {
		t.Errorf("the model shrank phase-1-refused prose by %.1f%%, want at least 10%%", pct)
	}
}

// TestPhase2a_KnownAnswersSurvive: each scenario is a passage an agent would send, a question about
// it, and the answer as it appears in the passage. After compression the answer's words must still be
// there, in order — numbers, paths, identifiers, names and negations included. Without a model call
// in CI this is what "the answer stays correct" can mean: the compressed context still contains it.
func TestPhase2a_KnownAnswersSurvive(t *testing.T) {
	dir := modelDir(t)
	r := NewCompressor(dir).Reduction(nil)
	raw, err := os.ReadFile("testdata/known_answers.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct{ Context, Question, Answer string }
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 12 {
		t.Fatalf("%d known-answer scenarios, want at least 12", len(cases))
	}
	var bytesIn, bytesOut, kept int
	for i, c := range cases {
		out, _, _, err := r.Reduce(context.Background(), []byte(c.Context), tare.KindProse)
		if err != nil {
			t.Fatal(err)
		}
		bytesIn += len(c.Context)
		bytesOut += len(out)
		if len(out) >= len(c.Context) {
			t.Errorf("scenario %d: not reduced — a known-answer check on unchanged text proves nothing", i)
		}
		if inOrder(string(out), strings.Fields(c.Answer)) {
			kept++
		} else {
			t.Errorf("scenario %d: %q — answer %q lost:\n%s", i, c.Question, c.Answer, out)
		}
	}
	t.Logf("MEASURED: %d known-answer scenarios, %d answers kept, %d → %d bytes (%.1f%% smaller)",
		len(cases), kept, bytesIn, bytesOut, 100*float64(bytesIn-bytesOut)/float64(bytesIn))
}

// inOrder reports whether every word appears in s as a word, in this order.
func inOrder(s string, words []string) bool {
	trim := func(w string) string { return strings.Trim(w, ".,;:()\"'") }
	i := 0
	for _, f := range strings.Fields(s) {
		if i < len(words) && trim(f) == trim(words[i]) {
			i++
		}
	}
	return i == len(words)
}
