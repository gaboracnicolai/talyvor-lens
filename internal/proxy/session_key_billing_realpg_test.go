package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/sessionkey"
)

// session_key_billing_realpg_test.go — W4.6.1 STEP 4b, MEASURED BEFORE IT IS BUILT.
//
// Step 4b is "the per-session spend bound", and step 4's own note records why it was deferred:
// "PER-SESSION SPEND BOUND: NOT BUILT, AND THE COLUMN IS ABSENT RATHER THAN PRESENT-AND-UNREAD".
//
// ⚠⚠ THAT UNDERSTATES IT, AND THIS FILE IS THE MEASUREMENT. A session key does not merely lack a
// BOUND. In the default configuration a session-key request moves NO LXC AT ALL — no hold, no
// settle, no shadow debit — so there is nothing for a bound to bound and nothing that bills the
// customer. Measured below on BOTH seams: zero lxc_ledger rows, zero lxc_reservations rows and an
// untouched balance, while the byte-for-byte equivalent request under a workspace-key identity
// bills 260,000 uLXC.
//
// ⚠ HONEST LIMIT — WHAT THIS FILE DOES NOT MEASURE. Whether the request is still recorded in
// token_events is NOT asserted here: costWireProxy's fixture does not create that table at all
// (probed, not assumed). So "metered but not billed" is only half-measured — the NOT BILLED half.
// Do not repeat the other half as if this file proved it.
//
// ⚠ WHY, AND IT IS ONE BRANCH. serve()'s ENTIRE LXC admission-and-debit block is inside
// `if agentKeyID != ""`, and agentKeyID is AuthContext.APIKeyID. internal/auth sets APIKeyID on the
// WORKSPACE-KEY branch and nowhere else — deliberately, and internal/auth's own test says why:
// "it keys the F4 per-agent LXC sub-budget allocator; a session key is not a scoped workspace key
// and must not enter that path". That refusal is right on its own terms. What nobody wrote down is
// that on the serve path the allocator is the ONLY thing that moves LXC by default, so
// "must not enter the sub-budget path" and "is never billed" are THE SAME BRANCH.
//
// ⚠ THE OTHER TWO MOVERS ARE OFF BY DEFAULT, WHICH IS WHY THE BRANCH IS THE WHOLE STORY. A census of
// every LXC value movement in internal/proxy finds exactly three — SpendLXCForAgent,
// ReserveLXCForAgent/SettleLXCReservation, and shadowSpendLXC's SpendLXC. The first two need a
// non-empty APIKeyID; the third is gated on LXCShadowSpendEnabled, which config.Load leaves FALSE,
// and its call sites sit in the `else` of reservationActive(), whose two flags both default TRUE.
//
// ⚠ WHY NO EXISTING TEST COULD SEE IT — THE FIXTURES ARE UNIFORM. Every serve-path billing test in
// this package stamps a NON-EMPTY APIKeyID ("agent-1", "agent-"+ws). The whole billing population is
// agent traffic, so no assertion has ever asked what a proxy request WITHOUT an APIKeyID bills. It
// is the same shape as the tier dot that drew "cheap" for every model outside a two-entry map: a
// default cannot be told from a hit when every subject is a hit.
//
// ⚠ SCOPE, STATED RATHER THAN IMPLIED: session-key ROUTES are off by default
// (LENS_SESSION_KEYS_ENABLED=false ⇒ never registered, every tlv_sk_ bearer refused), so this is
// LATENT on a default deployment, not live. It becomes live the moment step 4 is switched on, which
// is what step 6 (the chat screen) needs.
//
// ⚠ NOTHING IS FIXED HERE. Which accounting a session key should get is a decision: giving it a
// reservation identity contradicts migration 0122's documented refusal; debiting the workspace
// directly is the shadow path that is off for its own reasons; a self-accounting per-session bound
// caps the provider bill but still bills no customer. See the queue item.

// stubSessionKeyValidator satisfies internal/auth's session-key validator seam.
//
// ⚠ THE AuthContext UNDER TEST IS BUILT BY THE PRODUCT, NOT BY THIS FILE. A hand-written
// &auth.AuthContext{APIKeyID: ""} would make "the session credential carries no key id" an
// assumption of the fixture rather than a fact about internal/auth — and this repo has already
// measured what a fixture more permissive than the product proves (nothing). So the arm below runs
// the REAL auth.Manager.Authenticate and uses whatever AuthContext it returns.
type stubSessionKeyValidator struct{ sk *sessionkey.SessionKey }

func (s stubSessionKeyValidator) Validate(context.Context, string) (*sessionkey.SessionKey, error) {
	return s.sk, nil
}

// sessionKeyAuthContext returns the AuthContext the REAL auth.Manager produces for a session key.
func sessionKeyAuthContext(t *testing.T, wsID string) *auth.AuthContext {
	t.Helper()
	raw := sessionkey.KeyPrefix + "measured0000000000000000000000000"
	mgr := auth.NewManager("", nil, nil, nil).WithSessionKeys(stubSessionKeyValidator{sk: &sessionkey.SessionKey{
		ID:          "sk-1",
		WorkspaceID: wsID,
		UserID:      "user-1",
		ExpiresAt:   time.Now().Add(time.Hour),
	}})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	actx, err := mgr.Authenticate(req)
	if err != nil {
		t.Fatalf("the real auth.Manager refused a session key: %v — this measurement depends on it accepting one", err)
	}
	if actx.AuthMethod != auth.MethodSessionKey {
		t.Fatalf("AuthMethod = %q, want %q — the arm below would not be measuring a session key",
			actx.AuthMethod, auth.MethodSessionKey)
	}
	return actx
}

// driveWithAuth runs one request through the real handler under the given AuthContext.
// stream selects the seam: Lens's streaming lane is a second independent copy, so a finding proved
// on one seam says nothing about the other.
//
// ⚠ tag MAKES THE PROMPT UNIQUE PER ARM, AND IT IS LOAD-BEARING RATHER THAN TIDY. The first version
// sent a byte-identical body from both arms, so the second arm was a CACHE HIT: it never called
// upstream, released its hold and billed nothing — and the anti-blindness assertion caught it,
// reporting "THE HARNESS CANNOT SEE A BILL". Arm 1's zeros had been meaningless. Two arms that
// share a cache are one arm run twice.
func driveWithAuth(t *testing.T, p *Proxy, actx *auth.AuthContext, stream bool, tag string) int {
	t.Helper()
	prompt := tag + " " + strings.Repeat("x", 40000)
	body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]%s}`,
		prompt, map[bool]string{true: `,"stream":true`, false: ""}[stream])
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-log")
	req = req.WithContext(auth.WithAuthContext(req.Context(), actx))
	if stream {
		w := newFlushRecorder()
		p.HandleOpenAI(w, req)
		return w.Code
	}
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	return w.Code
}

// countingUpstream wraps an upstream and counts calls.
//
// ⚠ WITHOUT THIS, "NOTHING WAS BILLED" AND "NOTHING HAPPENED" ARE THE SAME OBSERVATION. A request
// short-circuited by the cache, or refused before the upstream call, also bills nothing — and would
// satisfy every zero-assertion below while proving nothing about the credential.
func countingUpstream(t *testing.T, inner *httptest.Server, n *int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(n, 1)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, inner.URL, r.Body)
		if err != nil {
			t.Errorf("counting upstream: %v", err)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("counting upstream: %v", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ledgerRowCount(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM lxc_ledger WHERE workspace_id=$1`, ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func reservationRowCount(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM lxc_reservations WHERE workspace_id=$1`, ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestMeasured_ASessionKeyRequestMovesNoLXC_Buffered is TWO-SIDED IN ONE PROCESS, and the order is
// deliberate: the session arm runs FIRST against a pristine funded balance, then the workspace arm
// runs on the SAME proxy and the SAME database. The second arm is the anti-blindness half — without
// it, "no ledger rows" would be a claim about this harness rather than about the credential.
//
// ⚠ EXPECTED TO GO RED WHEN STEP 4b LANDS. That is the fix arriving. Whoever makes a session-key
// request book something should DELETE the zero-assertions here and replace them with what it now
// books — and update the queue item, because its "the column is absent" framing is what this file
// corrects.
func TestMeasured_ASessionKeyRequestMovesNoLXC_Buffered(t *testing.T) {
	p, store, pool := costWireProxy(t)
	var upstreamCalls int64
	p.openAIURL = countingUpstream(t, usageUpstream(t, `{"prompt_tokens":10000,"completion_tokens":100}`), &upstreamCalls).URL

	// ── ARM 1: the session key, as the real auth.Manager builds it ────────────────
	sessionCtx := sessionKeyAuthContext(t, "ws-log")
	if sessionCtx.APIKeyID != "" {
		t.Fatalf("the session credential now carries APIKeyID %q. If a session key has been given a "+
			"key identity, it enters the agent allocator and this whole measurement is obsolete — "+
			"re-read migration 0122, which refuses exactly that.", sessionCtx.APIKeyID)
	}
	if code := driveWithAuth(t, p, sessionCtx, false, "session-arm"); code != http.StatusOK {
		t.Fatalf("session-key request status = %d, want 200 — it must be SERVED, or 'nothing billed' "+
			"would just mean 'nothing happened'", code)
	}

	if got := atomic.LoadInt64(&upstreamCalls); got != 1 {
		t.Fatalf("session-key request made %d upstream call(s), want 1 — a request that never reached "+
			"the provider bills nothing for an uninteresting reason, and every zero below would be vacuous", got)
	}

	if bal := seamBalance(t, store, "ws-log"); bal != costWireFunded {
		t.Errorf("MEASURED CHANGE: session-key balance = %d, want %d (untouched). If this is now less, "+
			"a session-key request has started billing — see the docstring; delete these assertions and "+
			"assert what it books.", bal, costWireFunded)
	}
	if n := ledgerRowCount(t, pool, "ws-log"); n != 0 {
		t.Errorf("MEASURED CHANGE: session-key request wrote %d lxc_ledger row(s), want 0", n)
	}
	if n := reservationRowCount(t, pool, "ws-log"); n != 0 {
		t.Errorf("MEASURED CHANGE: session-key request wrote %d lxc_reservations row(s), want 0 "+
			"(no hold is taken at all — agentReserveBlocks returns early on an empty key id)", n)
	}

	// ── ARM 2: the SAME request under a workspace-key identity — the anti-blindness half ──
	if code := driveWithAuth(t, p, &auth.AuthContext{APIKeyID: "agent-1", WorkspaceID: "ws-log"}, false, "workspace-arm"); code != http.StatusOK {
		t.Fatalf("workspace-key request status = %d, want 200", code)
	}
	flatULXC := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
	if bal := seamBalance(t, store, "ws-log"); bal != costWireFunded-flatULXC {
		t.Fatalf("THE HARNESS CANNOT SEE A BILL: workspace-key balance = %d, want %d. Until this arm "+
			"moves the balance, arm 1's zeros say nothing about session keys.", bal, costWireFunded-flatULXC)
	}
	if n := ledgerRowCount(t, pool, "ws-log"); n == 0 {
		t.Fatal("THE HARNESS CANNOT SEE A BILL: the workspace-key arm wrote no ledger rows either")
	}
}

// TestMeasured_ASessionKeyRequestMovesNoLXC_Streaming is the same measurement on the OTHER seam.
//
// ⚠ IT IS NOT A COPY FOR SYMMETRY. Lens's streaming lane is a second independent implementation, and
// this repo has already shipped a fix that landed on the buffered seam alone and was inert on every
// streamed request while the buffered test stayed green. A billing claim proved on one seam is a
// claim about that seam only.
func TestMeasured_ASessionKeyRequestMovesNoLXC_Streaming(t *testing.T) {
	p, store, pool := costWireProxy(t)
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":100}}\n\n" +
		"data: [DONE]\n\n"
	var upstreamCalls int64
	p.openAIURL = countingUpstream(t, sseUpstream(t, sse), &upstreamCalls).URL

	sessionCtx := sessionKeyAuthContext(t, "ws-log")
	if code := driveWithAuth(t, p, sessionCtx, true, "session-arm-stream"); code != http.StatusOK {
		t.Fatalf("streamed session-key request status = %d, want 200", code)
	}
	if got := atomic.LoadInt64(&upstreamCalls); got != 1 {
		t.Fatalf("streamed session-key request made %d upstream call(s), want 1 — every zero below "+
			"would otherwise be vacuous", got)
	}
	if bal := seamBalance(t, store, "ws-log"); bal != costWireFunded {
		t.Errorf("MEASURED CHANGE: streamed session-key balance = %d, want %d (untouched)", bal, costWireFunded)
	}
	if n := ledgerRowCount(t, pool, "ws-log"); n != 0 {
		t.Errorf("MEASURED CHANGE: streamed session-key request wrote %d lxc_ledger row(s), want 0", n)
	}

	// The anti-blindness half, on THIS seam — the streamed settle is a different code path from the
	// buffered one, so the buffered arm above cannot stand in for it.
	if code := driveWithAuth(t, p, &auth.AuthContext{APIKeyID: "agent-1", WorkspaceID: "ws-log"}, true, "workspace-arm-stream"); code != http.StatusOK {
		t.Fatalf("streamed workspace-key request status = %d, want 200", code)
	}
	flatULXC := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
	if bal := seamBalance(t, store, "ws-log"); bal != costWireFunded-flatULXC {
		t.Fatalf("THE HARNESS CANNOT SEE A BILL ON THE STREAMING SEAM: balance = %d, want %d. Until "+
			"this arm moves the balance, the streamed zeros above say nothing.", bal, costWireFunded-flatULXC)
	}
	if st := reservationStatus(t, pool, "ws-log"); st != "settled" {
		t.Fatalf("THE HARNESS CANNOT SEE A BILL ON THE STREAMING SEAM: reservation status = %q, want 'settled'", st)
	}
}
