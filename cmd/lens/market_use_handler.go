package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/workspace"
)

// B20.2 — USE A LISTING, PAY PER USE, AND THE SELLER EARNS (internal/market/use.go).
//
//	POST /v1/workspaces/{wsID}/marketplace/listings/{id}/use   {version, model, input, variables}
//	GET  /v1/workspaces/{wsID}/marketplace/earnings            the seller's payable, in holdback and available (µUSD)
//	GET  /v1/workspaces/{wsID}/marketplace/bill?month=2026-09  the buyer's billed uses in a month
//
// A use runs the listing through Lens's own proxy with the caller's credential, so the models it calls are
// billed to the buyer as usual; a paid listing's price then goes on the buyer's monthly marketplace bill.
// An agent's key may use a listing within its spending rules (403 when they refuse, naming the approval
// one needs).

type marketAgents interface {
	market.AgentJudge
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
}

func mountMarketUseRoutes(r chi.Router, store *market.Store, lens http.Handler, meter market.Meter, agents marketAgents) {
	r.Post("/v1/workspaces/{wsID}/marketplace/listings/{listingID}/use", func(w http.ResponseWriter, req *http.Request) {
		var in market.UseRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 256<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"version", "model", "input", "variables"}: `+err.Error())
			return
		}
		wsID := chi.URLParam(req, "wsID")
		agentID := ""
		if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" && agents != nil {
			a, ws, err := agents.AgentOfKey(req.Context(), actx.APIKeyID)
			switch {
			case err == nil && ws == wsID:
				agentID = a
			case err != nil && !errors.Is(err, economy.ErrAgentNotFound):
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		// B25.6: a test workspace's paid uses go on its Stripe test-mode bill.
		m, byKind := meter, stripeByKind{}
		if k, ok := meter.(stripeByKind); ok {
			m, byKind = k.meterFor(wsID), k
		}
		deps := market.UseDeps{Runner: proxyRunner{lens: lens, from: req}, Meter: m, Agents: agents}
		u, err := store.Use(req.Context(), deps, wsID, agentID, chi.URLParam(req, "listingID"), in)
		var need *economy.ApprovalNeededError
		var ran *runError
		switch {
		case errors.As(err, &need):
			writeJSONOK(w, http.StatusForbidden, map[string]any{"error": err.Error(), "approval_id": need.ApprovalID})
		case errors.Is(err, economy.ErrAgentRule), errors.Is(err, workspace.ErrMoneyWall):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case errors.Is(err, market.ErrNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, market.ErrTakenDown):
			writeJSONErr(w, http.StatusGone, err.Error())
		case errors.Is(err, market.ErrInvalid), errors.Is(err, market.ErrNoModel):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, market.ErrNotRunnable):
			writeJSONErr(w, http.StatusNotImplemented, err.Error())
		case errors.Is(err, market.ErrNoBill):
			if byKind.isTest != nil {
				if _, why := byKind.billFor(wsID); why != nil {
					writeJSONErr(w, http.StatusForbidden, why.Error())
					return
				}
			}
			writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
		case errors.As(err, &ran):
			// The buyer's own limits pass through (no credit, rate limited, a model their agent may not use). A
			// 401 cannot be the buyer's credential — it just passed the same authentication — so it is the
			// provider's, and a bad gateway.
			status := http.StatusBadGateway
			if ran.status >= 400 && ran.status < 500 && ran.status != http.StatusUnauthorized {
				status = ran.status
			}
			writeJSONErr(w, status, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			if u.MeterError != "" {
				slog.Warn("market: a use ran but is not yet on the buyer's bill; the next pass bills it",
					"use", u.ID, "workspace", wsID, "err", u.MeterError)
			}
			writeJSONOK(w, http.StatusOK, u)
		}
	})
	r.Get("/v1/workspaces/{wsID}/marketplace/earnings", func(w http.ResponseWriter, req *http.Request) {
		e, err := store.SellerEarnings(req.Context(), chi.URLParam(req, "wsID"), time.Now())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, e)
	})
	r.Get("/v1/workspaces/{wsID}/marketplace/bill", func(w http.ResponseWriter, req *http.Request) {
		month := time.Now().UTC()
		if m := req.URL.Query().Get("month"); m != "" {
			t, err := time.Parse("2006-01", m)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, "month must be YYYY-MM")
				return
			}
			month = t
		}
		b, err := store.MonthBill(req.Context(), chi.URLParam(req, "wsID"), month)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, b)
	})
}

// refundTakenDownMarketUses finishes what a takedown could not (B20.4): it refunds a use that was running
// when its listing came down, and retries a buyer's credit Stripe did not accept. refunder nil: no
// marketplace bill, so nothing was ever metered and there is nothing to credit.
func refundTakenDownMarketUses(ctx context.Context, store *market.Store, refunder market.Refunder) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if refunder == nil {
				continue
			}
			refunded, credited, err := store.RefundTakenDown(ctx, refunder)
			if err != nil {
				slog.Warn("market: refunding taken-down listings", "refunded", refunded, "credited", credited, "err", err)
			} else if refunded+credited > 0 {
				slog.Info("market: refunded taken-down listings", "refunded", refunded, "credited", credited)
			}
		}
	}
}

// meterPendingMarketUses bills, every few minutes, the uses whose meter event did not reach Stripe when
// they were used.
func meterPendingMarketUses(ctx context.Context, store *market.Store, meter market.Meter) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := store.MeterPending(ctx, meter, time.Minute); err != nil {
				slog.Warn("market: billing pending uses", "billed", n, "err", err)
			} else if n > 0 {
				slog.Info("market: billed pending uses", "billed", n)
			}
		}
	}
}

// proxyRunner runs a listing's model calls through Lens's own proxy routes as the caller — the same
// credential, so the same authentication, rules, cache and billing as the caller's own requests.
type proxyRunner struct {
	lens http.Handler
	from *http.Request
}

// runError is a model call the proxy did not answer with a 2xx.
type runError struct {
	status int
	body   string
}

func (e *runError) Error() string {
	return fmt.Sprintf("market: running the listing: Lens answered %d: %s", e.status, e.body)
}

func (p proxyRunner) Run(ctx context.Context, model string, messages []market.Message) (string, error) {
	var path string
	var body []byte
	anthropic := strings.HasPrefix(model, "claude")
	if anthropic {
		system, turns := "", []market.Message{}
		for _, m := range messages {
			if m.Role == "system" {
				system = m.Content
			} else {
				turns = append(turns, m)
			}
		}
		req := map[string]any{"model": model, "max_tokens": 4096, "messages": turns}
		if system != "" {
			req["system"] = system
		}
		body, _ = json.Marshal(req)
		path = "/v1/proxy/anthropic/v1/messages"
	} else {
		body, _ = json.Marshal(map[string]any{"model": model, "messages": messages})
		path = "/v1/proxy/openai/v1/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.RemoteAddr = p.from.RemoteAddr
	req.Header.Set("Content-Type", "application/json")
	for _, h := range []string{"Authorization", "X-Talyvor-Key", "X-Api-Key"} {
		if v := p.from.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set("X-Talyvor-Feature", "marketplace")
	rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
	p.lens.ServeHTTP(rec, req)
	if rec.status/100 != 2 {
		return "", &runError{status: rec.status, body: strings.TrimSpace(truncate(rec.buf.String(), 500))}
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.buf.Bytes(), &out); err != nil {
		return "", &runError{status: http.StatusBadGateway, body: "an unreadable answer: " + truncate(rec.buf.String(), 200)}
	}
	if !anthropic && len(out.Choices) > 0 {
		return out.Choices[0].Message.Content, nil
	}
	var text strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return text.String(), nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// bufferedResponse keeps an in-process response.
type bufferedResponse struct {
	header http.Header
	status int
	buf    bytes.Buffer
	wrote  bool
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(code int) {
	if !b.wrote {
		b.status, b.wrote = code, true
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.wrote = true
	return b.buf.Write(p)
}
