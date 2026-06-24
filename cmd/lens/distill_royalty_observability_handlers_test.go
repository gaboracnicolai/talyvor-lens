package main

// Real-PG proof for the distill royalty observability handlers (detect/margin) + the
// adversarial admin-gate sweep. Gated on LENS_TEST_DATABASE_URL. Reuses callJSON +
// fakeAuthenticator + testDetectorThresholds from the cache observability test (same package).
// distillMintHarness lives in internal/poolroyalty (not importable into package main), so
// this builds an equivalent cmd/lens harness — including the PR4 "drop the view first" fix.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/poolroyalty"
)

func distillObsHarness(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG distill observability test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		// Drop the dependent view FIRST (PR4 fix) — else DROP TABLE errors (2BP01) if a
		// leftover view from a reused DB depends on distill_royalty_mints.
		`DROP VIEW IF EXISTS distill_royalty_margin`,
		`DROP TABLE IF EXISTS distill_royalty_mints`,
		`CREATE TABLE distill_royalty_mints (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), request_id TEXT NOT NULL UNIQUE, contributor_workspace_id TEXT NOT NULL, requester_workspace_id TEXT NOT NULL, content_hash TEXT NOT NULL, avoided_cogs_usd DOUBLE PRECISION NOT NULL, minted_amount DOUBLE PRECISION NOT NULL, status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE VIEW distill_royalty_margin AS SELECT request_id, requester_workspace_id, contributor_workspace_id, content_hash, avoided_cogs_usd, minted_amount, avoided_cogs_usd - minted_amount AS margin_usd, status, created_at FROM distill_royalty_mints`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

type distillMintSeed struct {
	req, contrib, requester, content, status string
	avoided, minted                          float64
}

func (m distillMintSeed) insert(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	status := m.status
	if status == "" {
		status = "final"
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO distill_royalty_mints
		   (request_id, contributor_workspace_id, requester_workspace_id, content_hash, avoided_cogs_usd, minted_amount, status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		m.req, m.contrib, m.requester, m.content, m.avoided, m.minted, status); err != nil {
		t.Fatalf("seed %s: %v", m.req, err)
	}
}

func flaggedDistillVolume(r distillDetectResponse, content string) bool {
	for _, f := range r.Volume {
		if f.ContentHash == content && f.Flagged {
			return true
		}
	}
	return false
}

func flaggedDistillBilateral(r distillDetectResponse, c, req string) bool {
	for _, f := range r.Bilateral {
		if f.ContributorWorkspace == c && f.RequesterWorkspace == req && f.Flagged {
			return true
		}
	}
	return false
}

// /detect — swarm (content reused by many requesters) + tight bilateral pair → flagged;
// clean → not; AND the response has NO similarity key (distill has no similarity detector).
func TestDistillRoyaltyDetect_Integration(t *testing.T) {
	pool := distillObsHarness(t)
	h := newDistillRoyaltyDetectHandler(poolroyalty.NewDistillDetectorReader(pool, testDetectorThresholds()))

	// swarm (volume-flagged): content "hot", wsA reused by 4 distinct requesters.
	distillMintSeed{req: "sw1", contrib: "wsA", requester: "r1", content: "hot"}.insert(t, pool)
	distillMintSeed{req: "sw2", contrib: "wsA", requester: "r2", content: "hot"}.insert(t, pool)
	distillMintSeed{req: "sw3", contrib: "wsA", requester: "r3", content: "hot"}.insert(t, pool)
	distillMintSeed{req: "sw4", contrib: "wsA", requester: "r4", content: "hot"}.insert(t, pool)
	// bilateral-flagged: wsC→wsD ×3 across distinct contents (frac 1.0 each side).
	distillMintSeed{req: "bp1", contrib: "wsC", requester: "wsD", content: "c1"}.insert(t, pool)
	distillMintSeed{req: "bp2", contrib: "wsC", requester: "wsD", content: "c2"}.insert(t, pool)
	distillMintSeed{req: "bp3", contrib: "wsC", requester: "wsD", content: "c3"}.insert(t, pool)
	// clean: content "cool", wsE reused by only 2 requesters (volume-clean; bilateral frac 0.5).
	distillMintSeed{req: "cl1", contrib: "wsE", requester: "x1", content: "cool"}.insert(t, pool)
	distillMintSeed{req: "cl2", contrib: "wsE", requester: "x2", content: "cool"}.insert(t, pool)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/distill-royalty/detect?window=24h", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detect: code %d body=%s", rec.Code, rec.Body.String())
	}
	var resp distillDetectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// NO similarity key in the raw response (distill has no similarity detector).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if _, ok := raw["similarity"]; ok {
		t.Errorf("distill /detect must NOT have a similarity key; body=%s", rec.Body.String())
	}

	if !flaggedDistillVolume(resp, "hot") {
		t.Error(`"hot" must be volume-flagged (4 distinct requesters — swarm)`)
	}
	if flaggedDistillVolume(resp, "cool") {
		t.Error(`"cool" must NOT be volume-flagged (only 2 requesters)`)
	}
	if !flaggedDistillBilateral(resp, "wsC", "wsD") {
		t.Error("(wsC,wsD) must be bilateral-flagged (3 mints, frac 1.0)")
	}
	if flaggedDistillBilateral(resp, "wsE", "x1") {
		t.Error("(wsE,x1) must NOT be bilateral-flagged (frac 0.5)")
	}
}

// /margin — realized summary = FINAL only (held/revoked excluded), identity holds;
// ?by=content_hash → buckets; ?by=layer → 400 (cache-only dimension).
func TestDistillRoyaltyMargin_Integration(t *testing.T) {
	pool := distillObsHarness(t)
	h := newDistillRoyaltyMarginHandler(poolroyalty.NewDistillMarginReader(pool))

	distillMintSeed{req: "f1", contrib: "wsA", requester: "wsB", content: "d1", status: "final", avoided: 8, minted: 4}.insert(t, pool)
	distillMintSeed{req: "f2", contrib: "wsC", requester: "wsD", content: "d2", status: "final", avoided: 4, minted: 2}.insert(t, pool)
	// EXCLUDED:
	distillMintSeed{req: "hX", contrib: "wsA", requester: "wsB", content: "d3", status: "held", avoided: 100, minted: 50}.insert(t, pool)
	distillMintSeed{req: "rX", contrib: "wsA", requester: "wsB", content: "d4", status: "revoked", avoided: 100, minted: 50}.insert(t, pool)

	var resp marginResponse
	if code := callJSON(t, h, "/v1/admin/distill-royalty/margin", &resp); code != http.StatusOK {
		t.Fatalf("margin: code %d", code)
	}
	if resp.Summary.Mints != 2 || resp.Summary.AvoidedCOGSUSD != 12 || resp.Summary.MintedLENS != 6 || resp.Summary.MarginUSD != 6 {
		t.Errorf("summary mints=%d avoided=%v minted=%v margin=%v want 2/12/6/6 (final only)",
			resp.Summary.Mints, resp.Summary.AvoidedCOGSUSD, resp.Summary.MintedLENS, resp.Summary.MarginUSD)
	}
	if resp.Summary.MarginUSD != resp.Summary.AvoidedCOGSUSD-resp.Summary.MintedLENS {
		t.Error("margin identity broken")
	}

	var byResp marginResponse
	callJSON(t, h, "/v1/admin/distill-royalty/margin?by=content_hash", &byResp)
	if len(byResp.Breakdown) != 2 {
		t.Errorf("breakdown by content_hash: %d buckets want 2 (d1,d2)", len(byResp.Breakdown))
	}

	// layer is a CACHE-only dimension → 400 for distill.
	if code := callJSON(t, h, "/v1/admin/distill-royalty/margin?by=layer", nil); code != http.StatusBadRequest {
		t.Errorf("by=layer: code %d want 400 (cache-only dimension)", code)
	}
}

// ADVERSARIAL ADMIN-GATE — both distill endpoints × {non-admin, unauthenticated} → 401
// with NO data leaked.
func TestDistillRoyaltyObs_AdminGate(t *testing.T) {
	pool := distillObsHarness(t)
	distillMintSeed{req: "g1", contrib: "wsA", requester: "wsB", content: "d1", status: "final", avoided: 8, minted: 4}.insert(t, pool)

	endpoints := []struct {
		name, target string
		h            http.HandlerFunc
	}{
		{"detect", "/v1/admin/distill-royalty/detect", newDistillRoyaltyDetectHandler(poolroyalty.NewDistillDetectorReader(pool, testDetectorThresholds()))},
		{"margin", "/v1/admin/distill-royalty/margin", newDistillRoyaltyMarginHandler(poolroyalty.NewDistillMarginReader(pool))},
	}
	dataKeys := []string{"volume", "bilateral", "summary", "margin_usd", "avoided_cogs_usd", "minted_lens", "content_hash"}
	rejecters := []struct {
		name string
		a    fakeAuthenticator
	}{
		{"non-admin", fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: false, UserID: "ws-7"}}},
		{"unauthenticated", fakeAuthenticator{err: http.ErrNoCookie}},
	}

	for _, ep := range endpoints {
		for _, rj := range rejecters {
			rec := httptest.NewRecorder()
			requireAdmin(rj.a, http.HandlerFunc(ep.h))(rec, httptest.NewRequest(http.MethodGet, ep.target, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s/%s: code %d want 401", ep.name, rj.name, rec.Code)
			}
			body := rec.Body.String()
			for _, k := range dataKeys {
				if strings.Contains(body, k) {
					t.Errorf("%s/%s: 401 body LEAKED data key %q — body=%s", ep.name, rj.name, k, body)
				}
			}
		}
		rec := httptest.NewRecorder()
		requireAdmin(fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: true, UserID: "admin1"}}, http.HandlerFunc(ep.h))(rec, httptest.NewRequest(http.MethodGet, ep.target, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s/admin: code %d want 200", ep.name, rec.Code)
		}
	}
}
