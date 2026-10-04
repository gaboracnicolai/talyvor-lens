package kompress

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// modelDir is where the vendored weights are. The weights are 279 MB and are not in git: CI's
// "Tare phase 2a model" step fetches them at the pinned revision and sets LENS_TARE_MODEL_DIR and
// TARE_MODEL_REQUIRED=1, so there a missing model is a FAILURE, not a skip.
func modelDir(t testing.TB) string {
	t.Helper()
	dir := os.Getenv("LENS_TARE_MODEL_DIR")
	if dir == "" {
		dir = DefaultDir
	}
	if _, err := os.Stat(filepath.Join(dir, WeightsFile)); err != nil {
		if os.Getenv("TARE_MODEL_REQUIRED") == "1" {
			t.Fatalf("TARE_MODEL_REQUIRED=1 but the weights are not at %s: %v", dir, err)
		}
		t.Skipf("kompress-small weights not at %s (set LENS_TARE_MODEL_DIR)", dir)
	}
	return dir
}

var (
	loadOnce  sync.Once
	loadedM   *Model
	loadedT   *Tokenizer
	loadedErr error
)

func loaded(t testing.TB) (*Model, *Tokenizer) {
	t.Helper()
	dir := modelDir(t)
	loadOnce.Do(func() {
		loadedT, loadedErr = LoadTokenizer(filepath.Join(dir, TokenizerFile))
		if loadedErr == nil {
			loadedM, loadedErr = LoadModel(filepath.Join(dir, WeightsFile))
		}
	})
	if loadedErr != nil {
		t.Fatal(loadedErr)
	}
	return loadedM, loadedT
}

// TestGoldenMatchesONNX: testdata/golden.json holds, for two texts, the token ids the Hugging Face
// tokenizer produced and the logits onnxruntime computed from the PUBLISHED model.onnx. This port
// must reproduce both — the ids exactly, the logits to float32 rounding, the keep/drop decision on
// every token. The second text is 186 tokens, so the ±64 sliding window on the local layers is
// exercised, not just the global ones.
func TestGoldenMatchesONNX(t *testing.T) {
	m, tok := loaded(t)
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Text   string       `json:"text"`
		IDs    []int        `json:"ids"`
		Logits [][2]float64 `json:"logits"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 2 {
		t.Fatalf("golden.json has %d cases, want at least 2", len(cases))
	}
	for ci, c := range cases {
		ids := []int{clsID}
		for _, w := range strings.Fields(c.Text) {
			ids = append(ids, tok.EncodeWord(w)...)
		}
		ids = append(ids, sepID)
		if len(ids) != len(c.IDs) {
			t.Fatalf("case %d: %d ids, ONNX tokenizer gave %d\n got %v\nwant %v", ci, len(ids), len(c.IDs), ids, c.IDs)
		}
		for i := range ids {
			if ids[i] != c.IDs[i] {
				t.Fatalf("case %d: id %d is %d, want %d", ci, i, ids[i], c.IDs[i])
			}
		}
		got := m.Logits(ids)
		var worst float64
		for i := range got {
			for k := 0; k < 2; k++ {
				worst = math.Max(worst, math.Abs(float64(got[i][k])-c.Logits[i][k]))
			}
			if (got[i][1] > got[i][0]) != (c.Logits[i][1] > c.Logits[i][0]) {
				t.Errorf("case %d token %d: keep decision differs from ONNX (got %v, want %v)", ci, i, got[i], c.Logits[i])
			}
		}
		if worst > 2e-3 {
			t.Errorf("case %d: max |logit − ONNX logit| = %g, want ≤ 2e-3", ci, worst)
		}
		t.Logf("case %d: %d tokens, max |Δlogit| vs ONNX = %.2g", ci, len(ids), worst)
	}
}
