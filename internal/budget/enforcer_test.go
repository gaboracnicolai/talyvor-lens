package budget

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestEnforceOnBody_InjectsMaxTokensWhenAbsent(t *testing.T) {
	e := New(nil, BudgetPolicy{MaxOutputTokens: 100})
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)

	out, result, err := e.EnforceOnBody(context.Background(), "ws", body)
	if err != nil {
		t.Fatalf("EnforceOnBody: %v", err)
	}
	if !result.Rewritten {
		t.Errorf("Rewritten = false, want true (no max_tokens → should inject)")
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	got, ok := parsed["max_tokens"].(float64)
	if !ok || int(got) != 100 {
		t.Errorf("max_tokens = %v, want 100", parsed["max_tokens"])
	}
}

func TestEnforceOnBody_BelowLimitNoChange(t *testing.T) {
	e := New(nil, BudgetPolicy{MaxOutputTokens: 100})
	body := []byte(`{"model":"gpt-4","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)

	out, result, err := e.EnforceOnBody(context.Background(), "ws", body)
	if err != nil {
		t.Fatalf("EnforceOnBody: %v", err)
	}
	if result.Rewritten {
		t.Errorf("Rewritten = true, want false (request below limit)")
	}
	if string(out) != string(body) {
		t.Errorf("body should be unchanged when below limit")
	}
}

func TestEnforceOnBody_AboveLimitReducedToLimit(t *testing.T) {
	e := New(nil, BudgetPolicy{MaxOutputTokens: 100})
	body := []byte(`{"model":"gpt-4","max_tokens":200,"messages":[{"role":"user","content":"hi"}]}`)

	out, result, err := e.EnforceOnBody(context.Background(), "ws", body)
	if err != nil {
		t.Fatalf("EnforceOnBody: %v", err)
	}
	if !result.Rewritten {
		t.Errorf("Rewritten = false, want true (request above limit)")
	}

	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	if int(parsed["max_tokens"].(float64)) != 100 {
		t.Errorf("max_tokens = %v, want reduced to 100", parsed["max_tokens"])
	}
	// Other fields must be preserved.
	if parsed["model"] != "gpt-4" {
		t.Errorf("model field lost during rewrite")
	}
}

func TestEnforceOnBody_UnlimitedPolicyNoChange(t *testing.T) {
	e := New(nil, BudgetPolicy{MaxOutputTokens: 0})
	body := []byte(`{"max_tokens":99999,"messages":[{"role":"user","content":"hi"}]}`)

	out, result, err := e.EnforceOnBody(context.Background(), "ws", body)
	if err != nil {
		t.Fatalf("EnforceOnBody: %v", err)
	}
	if result.Rewritten {
		t.Errorf("Rewritten = true under unlimited policy; want false")
	}
	if string(out) != string(body) {
		t.Errorf("body should be unchanged under unlimited policy")
	}
}

func TestEnforceOnBody_RewrittenFlagIsCorrectInEachCase(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"absent", `{"messages":[]}`, true},
		{"below limit", `{"max_tokens":10,"messages":[]}`, false},
		{"at limit", `{"max_tokens":100,"messages":[]}`, false},
		{"above limit", `{"max_tokens":500,"messages":[]}`, true},
	}
	e := New(nil, BudgetPolicy{MaxOutputTokens: 100})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, result, err := e.EnforceOnBody(context.Background(), "ws", []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if result.Rewritten != tc.want {
				t.Errorf("Rewritten = %v, want %v", result.Rewritten, tc.want)
			}
		})
	}
}

func TestEnforceOnBody_ReasonDescribesTheChange(t *testing.T) {
	e := New(nil, BudgetPolicy{MaxOutputTokens: 100})

	_, injected, _ := e.EnforceOnBody(context.Background(), "ws", []byte(`{"messages":[]}`))
	if !strings.Contains(injected.Reason, "inject") {
		t.Errorf("injection Reason = %q, want it to mention 'inject'", injected.Reason)
	}

	_, reduced, _ := e.EnforceOnBody(context.Background(), "ws", []byte(`{"max_tokens":250,"messages":[]}`))
	if !strings.Contains(reduced.Reason, "reduced") {
		t.Errorf("reduction Reason = %q, want it to mention 'reduced'", reduced.Reason)
	}
	if !strings.Contains(reduced.Reason, "250") || !strings.Contains(reduced.Reason, "100") {
		t.Errorf("reduction Reason should reference both values; got %q", reduced.Reason)
	}
}

func TestEstimateInputTokens_ReturnsReasonableCount(t *testing.T) {
	e := New(nil, BudgetPolicy{})
	// Content is 40 chars total → ~10 tokens at len/4.
	body := []byte(`{"messages":[{"role":"user","content":"abcdefghijklmnopqrstuvwxyzabcdefghijklmn"}]}`)
	got := e.EstimateInputTokens(body)
	if got != 10 {
		t.Errorf("EstimateInputTokens = %d, want 10 (len/4 of 40-char content)", got)
	}
}

func TestEnforceOnBody_MalformedJSONReturnsError(t *testing.T) {
	e := New(nil, BudgetPolicy{MaxOutputTokens: 100})
	_, _, err := e.EnforceOnBody(context.Background(), "ws", []byte("{not valid json"))
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}
