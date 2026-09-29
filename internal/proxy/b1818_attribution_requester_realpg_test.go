package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/workspace"
)

// B18.18 — request_attribution says who made each request. A chat request — streamed, under the session
// token the suite mints for a signed-in person, whose user is their own workspace — records that user and
// the workspace's name as its author; an API request — buffered, under a workspace API key — records the
// key and its name. Asserted on the attribution rows, through the real handler.
func TestAttribution_AChatAndAnAPIRequestEachRecordTheirAuthorAndUser(t *testing.T) {
	pool := agentBankDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"+
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)

	send := func(stream bool, sessionID string, credential func(context.Context) context.Context) {
		t.Helper()
		p, _ := newDistillSpendProxy(t, &fakeDistillConv{}, "", workspace.DistillDisabled)
		p.openAIURL = upstream.URL + "/v1/chat/completions"
		p.router = nil
		p.SetAttributionStore(attribution.NewStore(pool))
		body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`
		if stream {
			body = `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hello"}]}`
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", "ws-log")
		req.Header.Set("X-Talyvor-Session", sessionID)
		req = req.WithContext(credential(req.Context()))
		w := httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("session %s: %d %s", sessionID, w.Code, w.Body.String())
		}
	}
	send(true, "b1818-chat", func(ctx context.Context) context.Context {
		return auth.WithAuthContext(ctx, &auth.AuthContext{WorkspaceID: "ws-log", UserID: "ws-log", AuthMethod: "jwt"})
	})
	send(false, "b1818-api", func(ctx context.Context) context.Context {
		return auth.WithAPIKey(ctx, &auth.APIKey{ID: "k-b1818", WorkspaceID: "ws-log", Name: "ci-bot", Active: true})
	})

	row := func(session string) (author, user string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := pool.QueryRow(context.Background(), `SELECT author, user_id FROM request_attribution WHERE session_id = $1`, session).Scan(&author, &user)
			if err == nil || time.Now().After(deadline) {
				if err != nil {
					t.Fatalf("no attribution row for %s: %v", session, err)
				}
				return author, user
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if author, user := row("b1818-chat"); author != "distill-spend" || user != "ws-log" {
		t.Errorf("chat request attributed to author %q, user %q; want the workspace's name and its user", author, user)
	}
	if author, user := row("b1818-api"); author != "ci-bot" || user != "key:k-b1818" {
		t.Errorf("API request attributed to author %q, user %q; want the key's name and id", author, user)
	}
}
