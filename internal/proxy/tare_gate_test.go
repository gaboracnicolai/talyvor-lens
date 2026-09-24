package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
)

// B6.4/B6.5 — Tare through the WIRE: what the provider receives, and what the spend row carries,
// on the buffered AND the streaming path.
//
// The assertions read the newest message's text as a SUFFIX of what the provider received, because
// the buffered path collapses a conversation into one user message upstream (pinned, and explained,
// by message_collapse_wire_test.go) while the streaming path forwards it as sent.

// tareToolOutput is the kind of message Tare exists for: a tool result of same-shaped rows.
func tareToolOutput() string {
	rows := make([]map[string]any, 40)
	for i := range rows {
		rows[i] = map[string]any{"test_name": fmt.Sprintf("TestCase%02d", i), "status": "passed", "duration_ms": 10 + i}
	}
	b, _ := json.Marshal(map[string]any{"results": rows})
	return string(b)
}

func newTareProxy(t *testing.T, policy workspace.TarePolicy) (*Proxy, *capturingUpstream, *recordingAlertSink) {
	t.Helper()
	up := &capturingUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		up.add(b)
		if bytes.Contains(b, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, anthropicUsageSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	exact, _ := newExactCacheForTest(t)
	wsm := workspace.New(nil)
	if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: "ws-tare", Name: "tare", Active: true, LoggingPolicy: workspace.LoggingMetadata, TarePolicy: policy,
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, wsm, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	pointEveryProviderAt(p, srv.URL)
	sink := &recordingAlertSink{}
	p.setAlertSink(sink)
	return p, up, sink
}

func dispatchTare(t *testing.T, p *Proxy, stream bool, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := map[string]any{
		"model":      "claude-haiku-4-5",
		"max_tokens": 64,
		"system":     "You summarise test runs.",
		"messages": []map[string]any{
			{"role": "user", "content": "Which tests ran?"},
			{"role": "assistant", "content": "Send me the output."},
			{"role": "user", "content": tareToolOutput()},
		},
	}
	if stream {
		req["stream"] = true
	}
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Talyvor-Workspace", "ws-tare")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	return w
}

func TestTare_OnReducesTheNewestMessageAndMetersIt_BothPaths(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			p, up, sink := newTareProxy(t, workspace.TareOptIn)
			w := dispatchTare(t, p, stream, map[string]string{"X-Talyvor-Tare": "true", "X-Talyvor-Issue": "TALYVOR-123"})

			if sent := up.lastPrompt(t); strings.Contains(sent, tareToolOutput()) {
				t.Fatalf("provider received the tool output verbatim with Tare on:\n%s", sent)
			}
			if got := w.Header().Get("X-Talyvor-Tare"); got != "applied" {
				t.Errorf("X-Talyvor-Tare response header = %q, want applied", got)
			}
			if len(sink.spends) != 1 {
				t.Fatalf("%d spend rows, want 1", len(sink.spends))
			}
			m := sink.spends[0].tare
			if m.Kind != "json" || m.TokensOut >= m.TokensIn || m.WorkItemID != "TALYVOR-123" {
				t.Errorf("spend row's Tare record = %+v, want kind json, tokens_out < tokens_in, work item TALYVOR-123", m)
			}
		})
	}
}

func TestTare_OffLeavesTheRequestUnchanged_BothPaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy workspace.TarePolicy
		hdr    map[string]string
	}{
		{"default policy, header set", workspace.DefaultTarePolicy, map[string]string{"X-Talyvor-Tare": "true"}},
		{"opt_in, no header", workspace.TareOptIn, nil},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				p, up, sink := newTareProxy(t, tc.policy)
				w := dispatchTare(t, p, stream, tc.hdr)
				if sent := up.lastPrompt(t); !strings.HasSuffix(sent, tareToolOutput()) {
					t.Errorf("provider received a changed newest message with Tare off:\n%s", sent)
				}
				if got := w.Header().Get("X-Talyvor-Tare"); got != "" {
					t.Errorf("X-Talyvor-Tare = %q with Tare off, want absent", got)
				}
				if len(sink.spends) != 1 || sink.spends[0].tare.Kind != "" {
					t.Errorf("spend rows = %+v, want one row with no Tare record", sink.spends)
				}
			})
		}
	}
}
