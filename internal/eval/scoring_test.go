package eval

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/quality"
)

// Regex scoring must take effect end-to-end through runTestCaseWith (not fall
// through to the heuristic scorer). Drives the staticScore wiring.
func TestRunTestCase_EvalRegexViaPipeline(t *testing.T) {
	srv := openAIMock(t, func(string) string { return "order id: 123-4567 confirmed" })
	p := newPipeline(nil, quality.New(nil), "k", "k", "k")
	p.openAIURL = srv.URL

	res := p.RunTestCase(context.Background(), TestCase{
		Name: "rx", Provider: "openai", Model: "gpt-4",
		Prompt: "give an order id", ExpectedOutput: `\d{3}-\d{4}`,
		EvalMethod: EvalRegex, PassThreshold: 1.0,
	})
	if res.Error != "" {
		t.Fatalf("error: %s", res.Error)
	}
	if res.Score != 1.0 || !res.Passed {
		t.Errorf("regex via pipeline: score=%v passed=%v, want 1.0/true", res.Score, res.Passed)
	}
}

// A malformed regex must surface as an error on the result, not a silent 0.
func TestRunTestCase_EvalRegexBadPatternSurfacesError(t *testing.T) {
	srv := openAIMock(t, func(string) string { return "anything" })
	p := newPipeline(nil, quality.New(nil), "k", "k", "k")
	p.openAIURL = srv.URL

	res := p.RunTestCase(context.Background(), TestCase{
		Name: "rxbad", Provider: "openai", Model: "gpt-4",
		Prompt: "x", ExpectedOutput: `(unclosed`,
		EvalMethod: EvalRegex, PassThreshold: 1.0,
	})
	if res.Error == "" {
		t.Error("malformed regex should set res.Error")
	}
}

func TestStaticScore_Exact(t *testing.T) {
	s, handled, err := staticScore(EvalExact, "hello", "hello")
	if !handled || err != nil || s != 1.0 {
		t.Fatalf("exact match: s=%v handled=%v err=%v", s, handled, err)
	}
	if s, _, _ := staticScore(EvalExact, "hello", "world"); s != 0 {
		t.Errorf("exact mismatch should score 0, got %v", s)
	}
}

func TestStaticScore_Contains(t *testing.T) {
	if s, _, _ := staticScore(EvalContains, "lo wor", "hello world"); s != 1.0 {
		t.Errorf("contains should score 1, got %v", s)
	}
	if s, _, _ := staticScore(EvalContains, "xyz", "hello world"); s != 0 {
		t.Errorf("not-contains should score 0, got %v", s)
	}
}

func TestStaticScore_Regex(t *testing.T) {
	if s, handled, err := staticScore(EvalRegex, `^\d{3}-\d{4}$`, "123-4567"); err != nil || !handled || s != 1.0 {
		t.Fatalf("regex match: s=%v handled=%v err=%v", s, handled, err)
	}
	if s, _, _ := staticScore(EvalRegex, `^\d+$`, "abc"); s != 0 {
		t.Errorf("regex non-match should score 0, got %v", s)
	}
	// A malformed regex is a configuration error, surfaced — not a silent 0.
	if _, _, err := staticScore(EvalRegex, `(unclosed`, "x"); err == nil {
		t.Error("malformed regex should return an error")
	}
}

func TestStaticScore_JSONValid(t *testing.T) {
	// Empty expected → "is the response valid JSON?"
	if s, handled, err := staticScore(EvalJSONSchema, "", `{"a":1,"b":[2,3]}`); err != nil || !handled || s != 1.0 {
		t.Fatalf("valid json: s=%v handled=%v err=%v", s, handled, err)
	}
	if s, _, _ := staticScore(EvalJSONSchema, "", `{not json`); s != 0 {
		t.Errorf("invalid json should score 0, got %v", s)
	}
}

func TestStaticScore_JSONSchemaRequiredKeys(t *testing.T) {
	schema := `{"name":"", "age":0, "active":false}`
	// All required keys present with compatible types → 1.
	if s, _, err := staticScore(EvalJSONSchema, schema, `{"name":"x","age":30,"active":true,"extra":1}`); err != nil || s != 1.0 {
		t.Fatalf("schema satisfied: s=%v err=%v", s, err)
	}
	// Missing a required key → 0.
	if s, _, _ := staticScore(EvalJSONSchema, schema, `{"name":"x","age":30}`); s != 0 {
		t.Errorf("missing key should score 0, got %v", s)
	}
	// Wrong type (age should be number) → 0.
	if s, _, _ := staticScore(EvalJSONSchema, schema, `{"name":"x","age":"thirty","active":true}`); s != 0 {
		t.Errorf("wrong type should score 0, got %v", s)
	}
	// A malformed schema is a configuration error.
	if _, _, err := staticScore(EvalJSONSchema, `{bad schema`, `{}`); err == nil {
		t.Error("malformed schema should return an error")
	}
}

func TestStaticScore_NotStaticMethod(t *testing.T) {
	// Heuristic + judge are NOT static — handled=false so the caller falls
	// through to the scorer / network path.
	if _, handled, _ := staticScore(EvalHeuristic, "", "x"); handled {
		t.Error("heuristic must not be handled by staticScore")
	}
	if _, handled, _ := staticScore(EvalLLMJudge, "", "x"); handled {
		t.Error("llm_judge must not be handled by staticScore")
	}
}
