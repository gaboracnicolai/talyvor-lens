package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// B8.3 — Tare is ON for a workspace that never set a policy, on the buffered AND the streaming path;
// X-Talyvor-Tare: false opts one request out; and prose the old compressor used to mangle passes
// through byte-for-byte because no reducer is sure about it.

func dispatchTareNewest(t *testing.T, p *Proxy, stream bool, newest string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := map[string]any{
		"model":      "claude-haiku-4-5",
		"max_tokens": 64,
		"messages":   []map[string]any{{"role": "user", "content": newest}},
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

func TestTare_OnByDefault_ReducesUnlessOptedOut_BothPaths(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			// "" is a workspace that never touched the setting.
			p, up, sink := newTareProxy(t, "")
			w := dispatchTareNewest(t, p, stream, tareToolOutput(), nil)
			if sent := up.lastPrompt(t); strings.Contains(sent, tareToolOutput()) {
				t.Errorf("default workspace: provider received the tool output verbatim — Tare is not on by default")
			}
			if got := w.Header().Get("X-Talyvor-Tare"); got != "applied" {
				t.Errorf("default workspace: X-Talyvor-Tare = %q, want applied", got)
			}
			if len(sink.spends) != 1 || sink.spends[0].tare.Kind != "json" {
				t.Errorf("default workspace: spend rows = %+v, want one row metering a json reduction", sink.spends)
			}

			p, up, sink = newTareProxy(t, "")
			w = dispatchTareNewest(t, p, stream, tareToolOutput(), map[string]string{"X-Talyvor-Tare": "false"})
			if sent := up.lastPrompt(t); !strings.HasSuffix(sent, tareToolOutput()) {
				t.Errorf("X-Talyvor-Tare: false: provider received a changed message:\n%s", sent)
			}
			if got := w.Header().Get("X-Talyvor-Tare"); got != "" {
				t.Errorf("X-Talyvor-Tare: false: response header = %q, want absent", got)
			}
			if len(sink.spends) != 1 || sink.spends[0].tare.Kind != "" {
				t.Errorf("X-Talyvor-Tare: false: spend rows = %+v, want one row with no Tare record", sink.spends)
			}

			p, up, _ = newTareProxy(t, "")
			w = dispatchTareNewest(t, p, stream, compressiblePrompt, nil)
			if sent := up.lastPrompt(t); !strings.HasSuffix(sent, compressiblePrompt) {
				t.Errorf("default workspace changed prose it must refuse to touch:\n got %q\nwant suffix %q", sent, compressiblePrompt)
			}
			if got := w.Header().Get("X-Talyvor-Tare"); got != "" {
				t.Errorf("prose: X-Talyvor-Tare = %q, want absent", got)
			}
		})
	}
}
