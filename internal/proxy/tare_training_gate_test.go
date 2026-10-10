package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/workspace"
)

// B27.36 — what Tare phase 2b's training set receives through the WIRE, on both paths: the newest message's prose
// from a workspace that opted in or is synthetic test traffic — and nothing from a workspace that did not opt in,
// a temporary chat, a chat kept out of the shared pool, or text phase 1 already reduces.

type recordingTraces struct {
	mu   sync.Mutex
	kept []string // "wsID|text"
}

func (r *recordingTraces) RecordTareTrace(wsID, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kept = append(r.kept, wsID+"|"+text)
}

func dispatchTraced(t *testing.T, p *Proxy, stream bool, wsID, content string, hdr map[string]string) {
	t.Helper()
	req := map[string]any{
		"model": "claude-haiku-4-5", "max_tokens": 64, "system": "You help with deploys.", "stream": stream,
		"messages": []map[string]any{{"role": "user", "content": content}},
	}
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Talyvor-Workspace", wsID)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestTareTraining_CollectsOnlyFromOptedInOrSyntheticWorkspaces_BothPaths(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			// Tare policy off: collecting for training does not depend on Tare running.
			p, _, _ := newTareProxy(t, workspace.TareDisabled)
			traces := &recordingTraces{}
			p.SetTareTraining(traces)
			ctx := context.Background()
			if err := p.workspaceManager.RegisterWorkspace(ctx, workspace.Workspace{ID: "ws-syn", Name: "s", Active: true, Synthetic: true}); err != nil {
				t.Fatal(err)
			}
			if err := p.workspaceManager.RegisterWorkspace(ctx, workspace.Workspace{ID: "ws-in", Name: "i", Active: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := p.workspaceManager.SetTareTraining(ctx, "ws-in", true, "user:owner"); err != nil {
				t.Fatal(err)
			}

			dispatchTraced(t, p, stream, "ws-tare", tareProse, nil) // never opted in
			dispatchTraced(t, p, stream, "ws-in", tareProse, map[string]string{"X-Talyvor-Cache-Store": "off"})
			dispatchTraced(t, p, stream, "ws-in", tareProse, map[string]string{"X-Talyvor-Pool": "off"})
			dispatchTraced(t, p, stream, "ws-in", tareToolOutput(), nil) // phase 1 reduces JSON
			dispatchTraced(t, p, stream, "ws-in", tareProse, nil)
			dispatchTraced(t, p, stream, "ws-syn", tareProse, nil)

			want := []string{"ws-in|" + tareProse, "ws-syn|" + tareProse}
			if fmt.Sprint(traces.kept) != fmt.Sprint(want) {
				t.Errorf("kept %q\nwant %q", traces.kept, want)
			}
			if p.workspaceManager.GetTareModel("ws-in") {
				t.Errorf("opting in to training turned the Tare model on")
			}
		})
	}
}
