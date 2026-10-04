package kompress

import (
	"context"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/tare"
)

// dropAll is a classifier that keeps nothing, so whatever survives was kept by the reducer's own rules.
type dropAll struct{}

func (dropAll) keep(_ context.Context, words []string) ([]bool, error) {
	return make([]bool, len(words)), nil
}

func (dropAll) tokens(string) int { return 1 }

func reduceWith(t *testing.T, c *Compressor, content string) (string, []string) {
	t.Helper()
	var reasons []string
	out, _, _, err := c.Reduction(func(r tare.Refusal) { reasons = append(reasons, r.Reason) }).
		Reduce(context.Background(), []byte(content), tare.KindProse)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), reasons
}

const prose = "We looked at the failing deploy together this afternoon and agreed that the simplest " +
	"thing to do is to wait for the platform team to finish their change before trying again, because " +
	"the job does not work while the old broker is still running in the background somewhere.\n"

// Lossless-only still holds for code: a fenced block inside a prose message goes upstream byte for
// byte while the prose around it is compressed, and numbers, paths, identifiers and negations in the
// prose are kept whatever the model says.
func TestReduce_CodeBlocksVerbatim_FactsAndNegationsKept(t *testing.T) {
	code := "```go\nfunc main() {\n\tfmt.Println(\"keep me exactly\")\n}\n```\n"
	in := prose + "\n" + code + "\nThe retry in internal/jobs/deploy.go uses runWithBackoff and waits 30 seconds, not 3.\n"
	out, reasons := reduceWith(t, newWithClassifier(dropAll{}), in)
	if len(out) >= len(in) {
		t.Fatalf("not reduced (reasons %v):\n%s", reasons, out)
	}
	if !strings.Contains(out, code) {
		t.Errorf("the fenced code block changed:\n%s", out)
	}
	for _, w := range []string{"internal/jobs/deploy.go", "runWithBackoff", "30", "not", "3."} {
		if !strings.Contains(out, w) {
			t.Errorf("%q dropped:\n%s", w, out)
		}
	}
}

// Content phase 1 refuses but that is not prose — JSON, source code — is refused whole: the model
// never runs on it.
func TestReduce_RefusesJSONAndCode(t *testing.T) {
	for name, in := range map[string]string{
		"json": `{"rows": [{"a": 1}, {"b": "a long string value that phase one could not table"}]}` + strings.Repeat(" ", 40),
		"code": strings.Repeat("if err := run(ctx); err != nil {\n\treturn fmt.Errorf(\"run: %w\", err)\n}\n", 8) + prose,
	} {
		out, reasons := reduceWith(t, newWithClassifier(dropAll{}), in)
		if out != in || len(reasons) != 1 || reasons[0] != tare.ReasonNotProse {
			t.Errorf("%s: want refused with %q, got reasons %v, changed=%v", name, tare.ReasonNotProse, reasons, out != in)
		}
	}
}

// No weights on disk: the request goes upstream unchanged and the refusal says why.
func TestReduce_NoWeightsRefuses(t *testing.T) {
	out, reasons := reduceWith(t, NewCompressor(t.TempDir()), prose+prose)
	if out != prose+prose || len(reasons) != 1 || reasons[0] != tare.ReasonModelUnavailable {
		t.Errorf("want unchanged with %q, got reasons %v", tare.ReasonModelUnavailable, reasons)
	}
}
