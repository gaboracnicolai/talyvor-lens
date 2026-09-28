package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B17.1 — SYNTHETIC ACCOUNTS THAT CAN NEVER TOUCH REAL MONEY OR REAL USERS.
//
// Two operator routes, registered ONLY when LENS_SYNTHETIC_KEY is set (otherwise a 404):
//
//	POST /v1/synthetic/workspaces        {"count":100} — creates that many synthetic workspaces, each
//	                                     with test credits and a token the harness uses as that user.
//	POST /v1/synthetic/workspaces/reset  — every synthetic workspace: its stored answers deleted, its
//	                                     test credits restored.
//
// The key is compared in constant time, calls are rate-limited, and every call — refused ones too —
// is written to synthetic_operations. What makes a workspace synthetic, and what that forbids, is in
// internal/workspace/synthetic.go and the proxy's partition.

const (
	syntheticKeyHeader = "X-Talyvor-Synthetic-Key"
	// syntheticCreditULXC is each synthetic workspace's test credit: 1,000 LXC, an admin grant, never
	// cash-backed.
	syntheticCreditULXC = int64(1_000) * 1_000_000
	syntheticTokenTTL   = 24 * time.Hour
	syntheticMaxCount   = 1000
	syntheticCallsPer   = time.Minute
	syntheticMaxCalls   = 10
)

// syntheticScopes is what a synthetic user's token may do: ask questions, read its analytics, and
// manage its keys and stored answers — what a signed-in owner does.
var syntheticScopes = []string{auth.ScopeProxy, auth.ScopeAnalytics, auth.ScopeKeys}

type syntheticWorkspaces interface {
	CreateSynthetic(ctx context.Context, id, name string) error
	ListSynthetic(ctx context.Context) ([]string, error)
}

type syntheticCredits interface {
	GrantLXC(ctx context.Context, workspaceID string, lxcAmount int64, reason string, metadata map[string]interface{}) (int64, error)
	GetLXCBalance(ctx context.Context, workspaceID string) (int64, error)
}

type syntheticAnswers interface {
	Delete(ctx context.Context, wsID string, scope storedanswers.Scope, by string, requestID int64) (storedanswers.Counts, error)
}

type syntheticAudit interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type syntheticDeps struct {
	workspaces syntheticWorkspaces
	credits    syntheticCredits
	answers    syntheticAnswers
	audit      syntheticAudit
	mint       tokenMinter
}

// callLimiter admits at most max calls in any window of per.
type callLimiter struct {
	mu    sync.Mutex
	max   int
	per   time.Duration
	calls []time.Time
}

func (l *callLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.calls[:0]
	for _, t := range l.calls {
		if now.Sub(t) < l.per {
			kept = append(kept, t)
		}
	}
	l.calls = kept
	if len(l.calls) >= l.max {
		return false
	}
	l.calls = append(l.calls, now)
	return true
}

func mountSyntheticRoutes(r chi.Router, key string, d syntheticDeps) {
	if key == "" {
		return
	}
	limit := &callLimiter{max: syntheticMaxCalls, per: syntheticCallsPer}
	r.Post("/v1/synthetic/workspaces", syntheticGuard(key, "create", limit, d, d.create))
	r.Post("/v1/synthetic/workspaces/reset", syntheticGuard(key, "reset", limit, d, d.reset))
}

// syntheticGuard checks the key and the rate, runs the action, and records the call either way.
func syntheticGuard(key, action string, limit *callLimiter, d syntheticDeps,
	run func(w http.ResponseWriter, r *http.Request) (int, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, outcome := 0, "ok"
		defer func() { d.record(r, action, n, outcome) }()
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(syntheticKeyHeader)), []byte(key)) != 1 {
			outcome = "unauthorized"
			writeJSONErr(w, http.StatusUnauthorized, "the synthetic operator key is required")
			return
		}
		if !limit.allow(time.Now()) {
			outcome = "rate_limited"
			w.Header().Set("Retry-After", "60")
			writeJSONErr(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d synthetic calls a minute", syntheticMaxCalls))
			return
		}
		n, outcome = run(w, r)
	}
}

func (d syntheticDeps) record(r *http.Request, action string, n int, outcome string) {
	if d.audit == nil {
		return
	}
	if _, err := d.audit.Exec(r.Context(),
		`INSERT INTO synthetic_operations (action, accounts, remote_addr, outcome) VALUES ($1, $2, $3, $4)`,
		action, n, r.RemoteAddr, outcome); err != nil {
		slog.Error("synthetic: audit write failed", "action", action, "outcome", outcome, "err", err)
	}
}

type syntheticUser struct {
	WorkspaceID string `json:"workspace_id"`
	Token       string `json:"token"`
	ExpiresAt   string `json:"expires_at"`
}

func (d syntheticDeps) create(w http.ResponseWriter, r *http.Request) (int, string) {
	in := struct {
		Count int `json:"count"`
	}{Count: 100}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return 0, "error: bad request"
		}
	}
	if in.Count < 1 || in.Count > syntheticMaxCount {
		writeJSONErr(w, http.StatusBadRequest, fmt.Sprintf("count must be 1 to %d", syntheticMaxCount))
		return 0, "error: bad count"
	}
	users := make([]syntheticUser, 0, in.Count)
	expires := time.Now().Add(syntheticTokenTTL).UTC().Format(time.RFC3339)
	for i := 0; i < in.Count; i++ {
		id, err := syntheticID()
		if err == nil {
			err = d.workspaces.CreateSynthetic(r.Context(), id, fmt.Sprintf("Synthetic user %s", id[1:7]))
		}
		if err == nil {
			_, err = d.credits.GrantLXC(r.Context(), id, syntheticCreditULXC, "synthetic test credits",
				map[string]interface{}{"synthetic": true})
		}
		var tok string
		if err == nil {
			tok, err = d.mint(id, id, syntheticScopes, syntheticTokenTTL)
		}
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, fmt.Sprintf("created %d, then: %v", len(users), err))
			return len(users), "error: " + err.Error()
		}
		users = append(users, syntheticUser{WorkspaceID: id, Token: tok, ExpiresAt: expires})
	}
	slog.Info("synthetic: workspaces created", "count", len(users))
	writeJSONOK(w, http.StatusCreated, map[string]any{"created": len(users), "workspaces": users})
	return len(users), "ok"
}

func (d syntheticDeps) reset(w http.ResponseWriter, r *http.Request) (int, string) {
	ids, err := d.workspaces.ListSynthetic(r.Context())
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	for i, id := range ids {
		if _, err := d.answers.Delete(r.Context(), id, storedanswers.ScopeAll, "synthetic-reset", 0); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, fmt.Sprintf("reset %d, then %s: %v", i, id, err))
			return i, "error: " + err.Error()
		}
		bal, err := d.credits.GetLXCBalance(r.Context(), id)
		if err == nil && bal < syntheticCreditULXC {
			_, err = d.credits.GrantLXC(r.Context(), id, syntheticCreditULXC-bal, "synthetic test credits restored",
				map[string]interface{}{"synthetic": true})
		}
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, fmt.Sprintf("reset %d, then %s: %v", i, id, err))
			return i, "error: " + err.Error()
		}
	}
	slog.Info("synthetic: workspaces reset", "count", len(ids))
	writeJSONOK(w, http.StatusOK, map[string]any{"reset": len(ids)})
	return len(ids), "ok"
}

// syntheticID is "s" and 26 random base32 characters — the shape of a provisioned "u…" id, never
// equal to one.
func syntheticID() (string, error) {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "s" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))[:26], nil
}

// refuseSynthetic answers 403 for a synthetic workspace: its credits are test credits, never bought,
// subscribed for or converted. Reports whether it refused.
func refuseSynthetic(w http.ResponseWriter, isSynthetic func(string) bool, wsID string) bool {
	if !isSynthetic(wsID) {
		return false
	}
	writeJSONErr(w, http.StatusForbidden, "a synthetic workspace has test credits only — it cannot buy, subscribe or convert")
	return true
}
