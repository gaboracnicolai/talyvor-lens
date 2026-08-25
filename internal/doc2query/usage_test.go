package doc2query

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ⚠ THIS ASSERTS A REAL HTTP ROUND TRIP, NOT A HAND-BUILT STRUCT. What is under test is a JSON
// tag: `usage.input_tokens`. A test that constructs the response struct in Go never decodes that
// tag and would pass over a misspelling of it, which is the only way this can realistically be
// wrong. So the fake serves bytes shaped like the API's.
//
// The numbers are deliberately unequal and non-round: a harness that reported input where output
// belongs, or summed the two, or returned max_tokens, gives a different answer to each.
const usageBody = `{"content":[{"text":"how do I do the thing?\nwhat causes this failure?"}],` +
	`"usage":{"input_tokens":817,"output_tokens":63}}`

func TestDeriveWithUsage_ReportsTheCountsTheAPIReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(usageBody))
	}))
	defer srv.Close()

	d := NewAnthropicDeriver("k", "claude-haiku-4-5")
	d.BaseURL = srv.URL
	qs, u, err := d.DeriveWithUsage(context.Background(), "an answer", 8)
	if err != nil {
		t.Fatalf("DeriveWithUsage: %v", err)
	}
	if len(qs) != 2 {
		t.Errorf("questions = %d (%v), want 2 — the usage assertions below mean nothing if the "+
			"reply was not actually parsed", len(qs), qs)
	}
	if u.InTokens != 817 || u.OutTokens != 63 {
		t.Errorf("usage = %+v, want {InTokens:817 OutTokens:63}", u)
	}
}

func TestAnswerWithUsage_ReportsTheCountsTheAPIReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(usageBody))
	}))
	defer srv.Close()

	d := NewAnthropicDeriver("k", "claude-haiku-4-5")
	d.BaseURL = srv.URL
	a, u, err := d.AnswerWithUsage(context.Background(), "a question")
	if err != nil {
		t.Fatalf("AnswerWithUsage: %v", err)
	}
	if a == "" {
		t.Error("answer is empty — usage without the thing it paid for is not a measurement")
	}
	if u.InTokens != 817 || u.OutTokens != 63 {
		t.Errorf("usage = %+v, want {InTokens:817 OutTokens:63}", u)
	}
}

// Derive and Answer keep their old signatures and must keep their old behaviour: they are the
// interface every existing caller uses, and the usage variants are additive.
func TestDeriveAndAnswer_StillReturnWhatTheyAlwaysDid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(usageBody))
	}))
	defer srv.Close()

	d := NewAnthropicDeriver("k", "")
	d.BaseURL = srv.URL
	qs, err := d.Derive(context.Background(), "an answer", 8)
	if err != nil || len(qs) != 2 {
		t.Errorf("Derive = %v, %v; want 2 questions and no error", qs, err)
	}
	a, err := d.Answer(context.Background(), "a question")
	if err != nil || a == "" {
		t.Errorf("Answer = %q, %v; want text and no error", a, err)
	}
}

// Usage.Add is what turns 154 per-call measurements into the per-answer figure W2.7 asks for.
func TestUsageAdd(t *testing.T) {
	var got Usage
	got.Add(Usage{InTokens: 3, OutTokens: 5})
	got.Add(Usage{InTokens: 7, OutTokens: 11})
	if got != (Usage{InTokens: 10, OutTokens: 16}) {
		t.Errorf("Add = %+v, want {10 16}", got)
	}
}
