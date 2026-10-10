package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
)

// B38.2 — GEMINI 3.8 FLASH IS CHARGED TODAY'S PRICE UNTIL 2027-01-01 AND THE NEW ONE FROM THAT DATE.
//
// https://ai.google.dev/gemini-api/docs/pricing (paid tier, read 2026-10-10): input "$0.75 through December 31,
// 2026" and "$1.50 starting January 1, 2027"; output $3.75 → $7.50. One streamed chat question goes through the
// real Google handler on each side of the date; the charge is read off the prepaid ledger row.
func TestB382_Gemini38FlashChargedThePriceInForceThatDay(t *testing.T) {
	cases := []struct {
		at                time.Time
		inPer1M, outPer1M float64
	}{
		{time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC), 0.75, 3.75},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 1.50, 7.50},
	}
	for _, tc := range cases {
		t.Run(tc.at.Format(time.DateOnly), func(t *testing.T) {
			t.Cleanup(catalog.SetClock(func() time.Time { return tc.at }))
			p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"},"finishReason":"STOP"}],`+
					`"usageMetadata":{"promptTokenCount":100000,"candidatesTokenCount":10000}}`+"\n\n")
			}))
			t.Cleanup(srv.Close)
			p.googleURL, p.googleKey = srv.URL, "google-key"

			body := `{"model":"gemini-3.8-flash","stream":true,"messages":[{"role":"user","content":"b382 ` + tc.at.Format(time.DateOnly) + `"}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/proxy/google/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Talyvor-Workspace", "ws-log")
			req = req.WithContext(auth.WithAuthContext(req.Context(), sessionKeyAuthContext(t, "ws-log")))
			w := newFlushRecorder()
			p.HandleGoogle(w, req)
			if got, _ := io.ReadAll(w.Result().Body); w.Code != http.StatusOK || !strings.Contains(string(got), "Hello") {
				t.Fatalf("status=%d body=%.300q — the model did not answer", w.Code, got)
			}

			want := settleULXC(100000*tc.inPer1M/1e6 + 10000*tc.outPer1M/1e6)
			if rows, debited, _ := prepaidDebits(t, pool); rows != 1 || debited != want {
				t.Errorf("prepaid ledger = %d row(s), %d µLXC; want 1 row of %d µLXC (100000 in + 10000 out at $%.2f/$%.2f per 1M)",
					rows, debited, want, tc.inPer1M, tc.outPer1M)
			}
		})
	}
}
