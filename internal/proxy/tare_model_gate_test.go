package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/talyvor/lens/internal/tare"
	"github.com/talyvor/lens/internal/workspace"
)

// B27.35 — Tare phase 2a through the WIRE. The model itself is internal/tare/kompress (measured there
// on the real weights); here a stand-in that halves the prose proves the GATE on both paths: it runs
// only for a workspace that opted in, only where phase 1 refused, and its saving is metered as prose.

const tareProse = "We looked at the failing deploy together this afternoon and agreed that the simplest thing " +
	"to do is to wait for the platform team to finish their change before trying again, because the job " +
	"still fails while the old broker keeps running in the background on the staging cluster."

type halvingModel struct{ calls atomic.Int32 }

func (h *halvingModel) Reduce(_ context.Context, content []byte, _ tare.Kind) ([]byte, int, int, error) {
	h.calls.Add(1)
	w := strings.Fields(string(content))
	out := []byte(strings.Join(w[:len(w)/2], " "))
	return out, tare.EstimateTokens(content), tare.EstimateTokens(out), nil
}

func dispatchTareContent(t *testing.T, p *Proxy, stream bool, content string) *httptest.ResponseRecorder {
	t.Helper()
	req := map[string]any{
		"model": "claude-haiku-4-5", "max_tokens": 64, "system": "You help with deploys.",
		"messages": []map[string]any{{"role": "user", "content": content}},
	}
	if stream {
		req["stream"] = true
	}
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Talyvor-Workspace", "ws-tare")
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	return w
}

func TestTareModel_OnlyForAnOptedInWorkspace_OnlyWherePhase1Refuses_BothPaths(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("opted in, prose/stream=%v", stream), func(t *testing.T) {
			p, up, sink := newTareProxy(t, workspace.TareAlways)
			m := &halvingModel{}
			p.SetTareModel(m)
			if err := p.workspaceManager.SetTareModel(context.Background(), "ws-tare", true); err != nil {
				t.Fatal(err)
			}
			w := dispatchTareContent(t, p, stream, tareProse)
			if sent := up.lastPrompt(t); strings.Contains(sent, tareProse) {
				t.Fatalf("provider received the prose verbatim with the model opted in:\n%s", sent)
			}
			if w.Header().Get("X-Talyvor-Tare") != "applied" || len(sink.spends) != 1 {
				t.Fatalf("X-Talyvor-Tare=%q, %d spend rows; want applied and 1", w.Header().Get("X-Talyvor-Tare"), len(sink.spends))
			}
			if got := sink.spends[0].tare; got.Kind != string(tare.KindProse) || got.TokensOut >= got.TokensIn {
				t.Errorf("spend row's Tare record = %+v, want kind prose with tokens_out < tokens_in", got)
			}
		})
		t.Run(fmt.Sprintf("not opted in, prose/stream=%v", stream), func(t *testing.T) {
			p, up, sink := newTareProxy(t, workspace.TareAlways)
			m := &halvingModel{}
			p.SetTareModel(m)
			dispatchTareContent(t, p, stream, tareProse)
			if sent := up.lastPrompt(t); !strings.HasSuffix(sent, tareProse) || m.calls.Load() != 0 {
				t.Errorf("model ran (%d calls) or the prose changed for a workspace that never opted in:\n%s", m.calls.Load(), sent)
			}
			if len(sink.spends) != 1 || sink.spends[0].tare.Kind != "" {
				t.Errorf("spend rows = %+v, want one row with no Tare record", sink.spends)
			}
		})
		t.Run(fmt.Sprintf("opted in, JSON phase 1 reduces/stream=%v", stream), func(t *testing.T) {
			p, _, sink := newTareProxy(t, workspace.TareAlways)
			m := &halvingModel{}
			p.SetTareModel(m)
			if err := p.workspaceManager.SetTareModel(context.Background(), "ws-tare", true); err != nil {
				t.Fatal(err)
			}
			dispatchTareContent(t, p, stream, tareToolOutput())
			if m.calls.Load() != 0 || len(sink.spends) != 1 || sink.spends[0].tare.Kind != string(tare.KindJSON) {
				t.Errorf("model calls=%d, spend rows=%+v; want 0 calls and phase 1's json reduction", m.calls.Load(), sink.spends)
			}
		})
	}
}
