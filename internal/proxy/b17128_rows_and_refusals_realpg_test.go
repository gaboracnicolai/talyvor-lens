package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// B17.128 — the 10 Oct testers' settings-tare-distill, reproduced. Forty same-shaped JSON rows were 1808 input
// tokens to Claude Sonnet 5 unreduced and 932 once Tare reduced them; the hold counted 1560 and 858, so the settle
// cut both charges to the hold. And an HTML document sent unconverted was refused 400 by the provider, and its
// hold stayed taken.

func b17128Rows() string {
	rows := make([]map[string]any, 40)
	for i := range rows {
		rows[i] = map[string]any{"id": i, "name": fmt.Sprintf("item %d", i), "status": "active", "region": "eu-west",
			"ref": fmt.Sprintf("404499mv1p4vpp-%d", i)}
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

func b17128Ask(t *testing.T, p *Proxy, stream bool, content string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"claude-sonnet-5","max_tokens":16,"stream":` + strconv.FormatBool(stream) +
		`,"messages":[{"role":"user","content":` + jsonString(content) + `}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "wsPoolA")
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "agent-wsPoolA", WorkspaceID: "wsPoolA"}))
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, req)
	return w
}

func TestB17128_FortyRowsAreChargedWhatTheProviderCounted(t *testing.T) {
	for _, tc := range []struct {
		tare   workspace.TarePolicy
		stream bool
	}{{workspace.TareAlways, false}, {workspace.TareAlways, true}, {workspace.TareDisabled, false}, {workspace.TareDisabled, true}} {
		stream := tc.stream
		t.Run(fmt.Sprintf("tare %s, stream %v", tc.tare, stream), func(t *testing.T) {
			p, pool, _ := anthropicPoolProxy(t)
			if err := p.workspaceManager.SetTarePolicy(context.Background(), "wsPoolA", tc.tare); err != nil {
				t.Fatal(err)
			}
			in := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				in = 1808 // what Claude counted for the rows as sent, and for them reduced by Tare
				if strings.Contains(string(body), `\"cols\"`) {
					in = 932
				}
				if strings.Contains(string(body), `"stream":true`) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, chatSSEWithUsage("forty rows", in, 16))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_b17128","type":"message","role":"assistant","model":"claude-sonnet-5",`+
					`"content":[{"type":"text","text":"forty rows"}],"stop_reason":"max_tokens",`+
					`"usage":{"input_tokens":`+strconv.Itoa(in)+`,"output_tokens":16}}`)
			}))
			t.Cleanup(up.Close)
			p.anthropicURL = up.URL

			w := b17128Ask(t, p, stream, b17128Rows())
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tared := w.Header().Get("X-Talyvor-Tare") == "applied"; tared != (tc.tare == workspace.TareAlways) || tared != (in == 932) {
				t.Fatalf("Tare %s: X-Talyvor-Tare %q, and the provider was sent the rows counted %d", tc.tare, w.Header().Get("X-Talyvor-Tare"), in)
			}
			usd, _ := alerts.CostUSDResolved("claude-sonnet-5", catalog.PurposeCharge, in, 0, 0, 16)
			want := int64(math.Ceil(usd / economy.LXCUSDValue * 1e6))
			charged, meta := spendRow(t, pool, "wsPoolA")
			if -charged != want {
				t.Fatalf("forty rows (%d input tokens) were charged %d µLXC, want %d at the catalog price", in, -charged, want)
			}
			if _, cut := meta["written_off_ulxc"]; cut {
				t.Errorf("the spend row was cut to its hold: %v", meta)
			}
		})
	}
}

func TestB17128_AProviderRefusalLeavesNothingHeld(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streamed"}[stream], func(t *testing.T) {
			p, pool, _ := anthropicPoolProxy(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":`+
					`"messages.0.content.0.document.source.base64.media_type: Input should be 'application/pdf'"}}`)
			}))
			t.Cleanup(up.Close)
			p.anthropicURL = up.URL

			if w := b17128Ask(t, p, stream, "What is the access code? Reply with it only."); w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want the provider's 400: %s", w.Code, w.Body.String())
			}
			var rows int
			var net int64
			if err := pool.QueryRow(context.Background(), `SELECT count(*), COALESCE(SUM(amount), 0) FROM lxc_ledger
				WHERE workspace_id = 'wsPoolA' AND type IN ('reservation_hold', 'reservation_release', 'spend', 'platform_fee')`).
				Scan(&rows, &net); err != nil {
				t.Fatal(err)
			}
			if rows == 0 {
				t.Fatal("the request took no hold, so this proves nothing about releasing one")
			}
			if net != 0 {
				t.Fatalf("after the provider refused, %d µLXC stayed taken across %d ledger rows", -net, rows)
			}
		})
	}
}
