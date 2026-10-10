package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B37.12 — A LONG PROMPT IS CHARGED THE LONG-PROMPT PRICE, ON THE WHOLE REQUEST.
//
// Gemini 3.1 Pro is $2.00 / $12.00 per 1M up to 200k prompt tokens and $4.00 / $18.00 above
// (https://ai.google.dev/gemini-api/docs/pricing, paid tier, read 2026-10-09). One streamed chat question
// goes through the real Google handler with the chat's credential; the upstream reports the prompt's
// tokens, and the charge is read off the prepaid ledger row against a figure computed here from that page.
func TestB3712_LongPromptChargedTheLongPromptPrice(t *testing.T) {
	cases := []struct {
		promptTokens      int
		inPer1M, outPer1M float64
	}{
		{250000, 4.00, 18.00},
		{150000, 2.00, 12.00},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.promptTokens), func(t *testing.T) {
			p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, `data: {"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"},"finishReason":"STOP"}],`+
					`"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":100}}`+"\n\n", tc.promptTokens)
			}))
			t.Cleanup(srv.Close)
			p.googleURL, p.googleKey = srv.URL, "google-key"

			// A prompt as long as the one the upstream counts (~4 characters a token).
			q := jsonString(fmt.Sprintf("b3712-%d %s", tc.promptTokens, strings.Repeat("word ", tc.promptTokens*4/5)))
			body := `{"model":"gemini-3.1-pro-preview","stream":true,"messages":[{"role":"user","content":` + q + `}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/proxy/google/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Talyvor-Workspace", "ws-log")
			req = req.WithContext(auth.WithAuthContext(req.Context(), sessionKeyAuthContext(t, "ws-log")))
			w := newFlushRecorder()
			p.HandleGoogle(w, req)
			if got, _ := io.ReadAll(w.Result().Body); w.Code != http.StatusOK || !strings.Contains(string(got), "Hello") {
				t.Fatalf("status=%d body=%.300q — the model did not answer", w.Code, got)
			}

			want := settleULXC(float64(tc.promptTokens)*tc.inPer1M/1e6 + 100*tc.outPer1M/1e6)
			if rows, debited, _ := prepaidDebits(t, pool); rows != 1 || debited != want {
				t.Errorf("prepaid ledger = %d row(s), %d µLXC; want 1 row of %d µLXC (%d in + 100 out at $%.2f/$%.2f per 1M)",
					rows, debited, want, tc.promptTokens, tc.inPer1M, tc.outPer1M)
			}
		})
	}
}

// B37.15 — Claude Haiku 5.5 is $0.10 / $0.50 per 1M for prompts up to 100,000 tokens and $0.50 / $2.50 above,
// on the whole request (https://platform.claude.com/docs/en/about-claude/pricing, read 2026-10-10). One
// streamed chat question goes through the real Anthropic handler; the charge is read off the prepaid ledger row.
func TestB3715_Haiku55LongPromptChargedTheLongPromptPrice(t *testing.T) {
	cases := []struct {
		promptTokens      int
		inPer1M, outPer1M float64
	}{
		{150000, 0.50, 2.50},
		{50000, 0.10, 0.50},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.promptTokens), func(t *testing.T) {
			p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, chatSSEWithUsage("Hello", tc.promptTokens, 100))
			}))
			t.Cleanup(srv.Close)
			p.anthropicURL = srv.URL

			q := jsonString(fmt.Sprintf("b3715-%d %s", tc.promptTokens, strings.Repeat("word ", tc.promptTokens*4/5)))
			body := `{"model":"claude-haiku-5-5","max_tokens":4096,"stream":true,"messages":[{"role":"user","content":` + q + `}]}`
			res := askNewestModel(t, p, true, true, body)
			if got, _ := io.ReadAll(res.Body); res.StatusCode != http.StatusOK || !strings.Contains(string(got), "Hello") {
				t.Fatalf("status=%d body=%.300q — the model did not answer", res.StatusCode, got)
			}

			want := settleULXC(float64(tc.promptTokens)*tc.inPer1M/1e6 + 100*tc.outPer1M/1e6)
			if rows, debited, _ := prepaidDebits(t, pool); rows != 1 || debited != want {
				t.Errorf("prepaid ledger = %d row(s), %d µLXC; want 1 row of %d µLXC (%d in + 100 out at $%.2f/$%.2f per 1M)",
					rows, debited, want, tc.promptTokens, tc.inPer1M, tc.outPer1M)
			}
		})
	}
}

// B38.4 — GPT-6.1 Sol is $2.00 / $10.00 per 1M for prompts up to 272,000 input tokens and $4.00 / $15.00 above,
// on the whole request (https://developers.openai.com/api/docs/pricing, read 2026-10-10). One streamed chat
// question goes through the real OpenAI handler; the charge is read off the prepaid ledger row.
func TestB384_GPT61SolLongPromptChargedTheLongPromptPrice(t *testing.T) {
	cases := []struct {
		promptTokens      int
		inPer1M, outPer1M float64
	}{
		{300000, 4.00, 15.00},
		{100000, 2.00, 10.00},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.promptTokens), func(t *testing.T) {
			p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"+
					"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":100}}\n\ndata: [DONE]\n\n", tc.promptTokens)
			}))
			t.Cleanup(srv.Close)
			p.openAIURL = srv.URL

			q := jsonString(fmt.Sprintf("b384-%d %s", tc.promptTokens, strings.Repeat("word ", tc.promptTokens*4/5)))
			body := `{"model":"gpt-6.1-sol","stream":true,"messages":[{"role":"user","content":` + q + `}]}`
			res := askNewestModel(t, p, false, true, body)
			if got, _ := io.ReadAll(res.Body); res.StatusCode != http.StatusOK || !strings.Contains(string(got), "Hello") {
				t.Fatalf("status=%d body=%.300q — the model did not answer", res.StatusCode, got)
			}

			want := settleULXC(float64(tc.promptTokens)*tc.inPer1M/1e6 + 100*tc.outPer1M/1e6)
			if rows, debited, _ := prepaidDebits(t, pool); rows != 1 || debited != want {
				t.Errorf("prepaid ledger = %d row(s), %d µLXC; want 1 row of %d µLXC (%d in + 100 out at $%.2f/$%.2f per 1M)",
					rows, debited, want, tc.promptTokens, tc.inPer1M, tc.outPer1M)
			}
		})
	}
}
