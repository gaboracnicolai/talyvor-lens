package pairverify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Only a bare YES may allow a serve; every other reply refuses.
func TestParse_OnlyABareYesAllowsTheServe(t *testing.T) {
	for raw, want := range map[string]bool{
		"YES": true, "yes": true, " Yes.\n": true,
		"NO": false, "Yes, but only on v2": false, "": false, "I can't tell": false, "YES NO": false,
	} {
		if got := Parse(raw); got != want {
			t.Errorf("Parse(%q) = %v, want %v", raw, got, want)
		}
	}
}

// The client sends both questions at temperature 0 and returns the verdict with its token usage.
func TestAnthropicVerifier_SendsThePairAndReturnsUsage(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sent)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"NO"}],"usage":{"input_tokens":120,"output_tokens":1}}`)
	}))
	defer srv.Close()
	v := NewAnthropicVerifier("k", "")
	v.URL = srv.URL

	got, err := v.Verify(context.Background(), "How do I enable SSO?", "How do I disable SSO?")
	if err != nil {
		t.Fatal(err)
	}
	if got.Same || got.InTokens != 120 || got.OutTokens != 1 {
		t.Errorf("verdict = %+v, want Same=false with 120 in / 1 out", got)
	}
	if sent["temperature"] != float64(0) || sent["model"] != defaultModel {
		t.Errorf("request temperature/model = %v/%v, want 0/%s", sent["temperature"], sent["model"], defaultModel)
	}
	msg := sent["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(msg, "How do I enable SSO?") || !strings.Contains(msg, "How do I disable SSO?") {
		t.Errorf("prompt does not carry both questions: %q", msg)
	}
}
