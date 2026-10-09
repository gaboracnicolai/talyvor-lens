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
