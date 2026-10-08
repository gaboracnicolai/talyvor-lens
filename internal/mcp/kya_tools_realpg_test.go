package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/kya"
)

// B30.5 — an agent shows its own Know Your Agent credential with its own key, and a frozen agent has none.
func TestWalletCredential_AnAgentShowsItsOwnCredential(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	a, err := store.CreateAgent(ctx, "ws-kya", "Scout", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, "ws-kya", a.ID, "key-scout"); err != nil {
		t.Fatal(err)
	}
	key, _, err := kya.ParseKey("")
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAgentBank(store)
	srv.SetCredentials(kya.New(store, key, time.Hour))
	call := func() (bool, string) {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wallet_credential","arguments":{}}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-scout", WorkspaceID: "ws-kya"}))
		w := httptest.NewRecorder()
		srv.HandleRPC(w, req)
		var resp struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Result.Content) == 0 {
			t.Fatalf("wallet_credential = %s", w.Body.String())
		}
		return resp.Result.IsError, resp.Result.Content[0].Text
	}

	refused, text := call()
	var c kya.Credential
	if err := json.Unmarshal([]byte(text), &c); refused || err != nil || c.Claims.Subject != a.ID || c.Claims.Agent.Name != "Scout" ||
		strings.Count(c.Token, ".") != 2 {
		t.Fatalf("wallet_credential = %s; want the agent's own signed credential", text)
	}
	if err := store.PauseAgent(ctx, "ws-kya", a.ID, "owner froze it"); err != nil {
		t.Fatal(err)
	}
	if refused, text := call(); !refused || !strings.Contains(text, "frozen") {
		t.Fatalf("a frozen agent's wallet_credential = %s; want refused, saying it is frozen", text)
	}
}
