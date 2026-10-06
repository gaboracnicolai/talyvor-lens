package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// B35.5 — step 5 of the TypeScript and Python quickstarts shows the statement Lens writes for the quickstart's
// model call. This runs the quickstart's money on a migrated schema through the real handler, with the defaults
// a Lens starts on (agent allocation and reservations on) and the platform fee wired as main wires it: the
// agent is funded 10 LXC, asks gpt-4o-mini "Hello" with its own key, and its statement is read as the statement
// route reads it. Each README's step 5 must list exactly those lines, newest first: kind, amount and balance.

var b355Line = regexp.MustCompile(`"?kind"?: "(\w+)", "?amount_ulxc"?: (-?\d+), "?balance_after_ulxc"?: (-?\d+)`)

func b355Statement(t *testing.T) []economy.AgentStatementLine {
	t.Helper()
	const key = "key-b355"
	pool := agentBankDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	b3211Fee(store)
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	p.router = nil
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	// What OpenAI answers gpt-4o-mini's "Hello".
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-b355","object":"chat.completion","model":"gpt-4o-mini-2024-07-18",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"Hello! How can I assist you today?"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":8,"completion_tokens":9,"total_tokens":17}}`)
	}))
	t.Cleanup(up.Close)
	p.openAIURL = up.URL

	seamFund(t, pool, b2313WS, b2313Funded)
	agent, err := store.CreateAgent(ctx, b2313WS, "researcher", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, b2313WS, agent.ID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, b2313WS, agent.ID, 10_000_000); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", b2313WS)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: key, WorkspaceID: b2313WS}))
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("the agent's call: status %d, want 200; body=%s", w.Code, w.Body.String())
	}
	lines, err := store.AgentStatement(ctx, b2313WS, agent.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestB355_TheSDKQuickstartsShowTheStatementLensWrites(t *testing.T) {
	lines := b355Statement(t)
	var wrote []string
	for _, l := range lines {
		wrote = append(wrote, fmt.Sprintf("%s %d → %d", l.Kind, l.AmountULXC, l.BalanceAfterULXC))
	}
	for _, sdk := range []string{"typescript", "python"} {
		readme, err := os.ReadFile(filepath.Join("..", "..", "sdk", sdk, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		quick := string(readme)
		if i := strings.Index(quick, "\n## Quick start"); i >= 0 {
			quick = quick[i+1:]
		}
		if j := strings.Index(quick[3:], "\n## "); j >= 0 {
			quick = quick[:j+3]
		}
		var shown []string
		for _, m := range b355Line.FindAllStringSubmatch(quick, -1) {
			shown = append(shown, m[1]+" "+m[2]+" → "+m[3])
		}
		if strings.Join(shown, "; ") != strings.Join(wrote, "; ") {
			t.Errorf("sdk/%s/README.md's quickstart shows the statement\n  %s\nbut Lens wrote\n  %s",
				sdk, strings.Join(shown, "; "), strings.Join(wrote, "; "))
		}
	}
}
