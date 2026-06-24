package main

// Real-PG proof for the Pool-B royalty observability handlers (detect/resolve/margin)
// + the adversarial admin-gate sweep. Gated on LENS_TEST_DATABASE_URL (these are SQL
// readers; pgxmock won't prove them). Reuses fakeAuthenticator from
// adjudication_handler_test.go (same package).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/poolroyalty"
)

// testDetectorThresholds — LOW thresholds so a handful of seeded rows trips the flags.
func testDetectorThresholds() poolroyalty.DetectorThresholds {
	return poolroyalty.DetectorThresholds{
		VolumeMinMints: 3, VolumeMaxRequesters: 1, // entry >=3 mints AND <=1 distinct requester
		BilateralMinFrac: 0.9, BilateralMinMints: 3, // pair >=3 mints AND >=90% of each side's flow
		SimilarityMinSample: 3, SimilarityMaxStddev: 0.05, // >=3 tight semantic hits
	}
}

func obsHarness(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG observability test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP VIEW IF EXISTS pool_royalty_margin`,
		`DROP TABLE IF EXISTS pool_royalty_mints`,
		`CREATE TABLE pool_royalty_mints (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), request_id TEXT NOT NULL UNIQUE, requester_workspace_id TEXT NOT NULL, contributor_workspace_id TEXT NOT NULL, layer TEXT NOT NULL, entry_id TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', similarity DOUBLE PRECISION NOT NULL DEFAULT 0, avoided_cogs_usd DOUBLE PRECISION NOT NULL DEFAULT 0, minted_amount DOUBLE PRECISION NOT NULL DEFAULT 0, answer_sha256 TEXT NOT NULL DEFAULT '', prompt_sha256 TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'final', finalize_after TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE VIEW pool_royalty_margin AS SELECT request_id, requester_workspace_id, contributor_workspace_id, layer, provider, model, avoided_cogs_usd, minted_amount, avoided_cogs_usd - minted_amount AS margin_usd, status, created_at FROM pool_royalty_mints`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

type mintSeed struct {
	req, contrib, requester, entry, layer, status, prompt string
	sim, avoided, minted                                  float64
	finalizeAfter                                         *time.Time
}

func (m mintSeed) insert(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	layer := m.layer
	if layer == "" {
		layer = "exact"
	}
	status := m.status
	if status == "" {
		status = "final"
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO pool_royalty_mints
		   (request_id, contributor_workspace_id, requester_workspace_id, entry_id, layer, status, prompt_sha256, similarity, avoided_cogs_usd, minted_amount, finalize_after)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		m.req, m.contrib, m.requester, m.entry, layer, status, m.prompt, m.sim, m.avoided, m.minted, m.finalizeAfter); err != nil {
		t.Fatalf("seed %s: %v", m.req, err)
	}
}

// callJSON drives the INNER handler directly (the admin gate is proven separately).
func callJSON(t *testing.T, h http.HandlerFunc, target string, out any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode (%d): %v body=%s", rec.Code, err, rec.Body.String())
		}
	}
	return rec.Code
}

func flaggedVolume(r detectResponse, entry string) bool {
	for _, f := range r.Volume {
		if f.EntryID == entry && f.Flagged {
			return true
		}
	}
	return false
}

func flaggedBilateral(r detectResponse, c, req string) bool {
	for _, f := range r.Bilateral {
		if f.ContributorWorkspace == c && f.RequesterWorkspace == req && f.Flagged {
			return true
		}
	}
	return false
}

func flaggedSimilarity(r detectResponse, c, entry string) bool {
	for _, f := range r.Similarity {
		if f.ContributorWorkspace == c && f.EntryID == entry && f.Flagged {
			return true
		}
	}
	return false
}

// /detect — volume-concentrated + bilateral pair + similarity cluster → Flagged=true;
// clean traffic → Flagged=false.
func TestPoolRoyaltyDetect_Integration(t *testing.T) {
	pool := obsHarness(t)
	h := newPoolRoyaltyDetectHandler(poolroyalty.NewDetectorReader(pool, testDetectorThresholds()))

	// volume-flagged: entry E1, wsA→wsB ×3 (3 mints, 1 distinct requester).
	mintSeed{req: "v1", contrib: "wsA", requester: "wsB", entry: "E1"}.insert(t, pool)
	mintSeed{req: "v2", contrib: "wsA", requester: "wsB", entry: "E1"}.insert(t, pool)
	mintSeed{req: "v3", contrib: "wsA", requester: "wsB", entry: "E1"}.insert(t, pool)
	// bilateral-flagged: wsC→wsD ×3 across distinct entries (frac 1.0 each side).
	mintSeed{req: "b1", contrib: "wsC", requester: "wsD", entry: "E3"}.insert(t, pool)
	mintSeed{req: "b2", contrib: "wsC", requester: "wsD", entry: "E4"}.insert(t, pool)
	mintSeed{req: "b3", contrib: "wsC", requester: "wsD", entry: "E5"}.insert(t, pool)
	// clean: entry E2, wsE→{wsF,wsG,wsH} (3 distinct requesters → volume-clean; pair frac 0.33 → bilateral-clean).
	mintSeed{req: "c1", contrib: "wsE", requester: "wsF", entry: "E2"}.insert(t, pool)
	mintSeed{req: "c2", contrib: "wsE", requester: "wsG", entry: "E2"}.insert(t, pool)
	mintSeed{req: "c3", contrib: "wsE", requester: "wsH", entry: "E2"}.insert(t, pool)
	// similarity-flagged: semantic cluster (wsS, ES), 3 distinct prompts at tight similarity.
	mintSeed{req: "sm1", contrib: "wsS", requester: "wsT", entry: "ES", layer: "semantic", prompt: "p1", sim: 0.95}.insert(t, pool)
	mintSeed{req: "sm2", contrib: "wsS", requester: "wsU", entry: "ES", layer: "semantic", prompt: "p2", sim: 0.95}.insert(t, pool)
	mintSeed{req: "sm3", contrib: "wsS", requester: "wsV", entry: "ES", layer: "semantic", prompt: "p3", sim: 0.96}.insert(t, pool)

	var resp detectResponse
	if code := callJSON(t, h, "/v1/admin/pool-royalty/detect?window=24h", &resp); code != http.StatusOK {
		t.Fatalf("detect: code %d", code)
	}
	if !flaggedVolume(resp, "E1") {
		t.Error("E1 must be volume-flagged (3 mints, 1 requester)")
	}
	if flaggedVolume(resp, "E2") {
		t.Error("E2 must NOT be volume-flagged (3 distinct requesters)")
	}
	if !flaggedBilateral(resp, "wsC", "wsD") {
		t.Error("(wsC,wsD) must be bilateral-flagged (3 mints, frac 1.0)")
	}
	if flaggedBilateral(resp, "wsE", "wsF") {
		t.Error("(wsE,wsF) must NOT be bilateral-flagged (frac 0.33)")
	}
	if !flaggedSimilarity(resp, "wsS", "ES") {
		t.Error("(wsS,ES) must be similarity-flagged (tight semantic cluster)")
	}
}

// /resolve — held mints on a tuple resolve to exactly those request_ids + the label;
// bad type → 400.
func TestPoolRoyaltyResolve_Integration(t *testing.T) {
	pool := obsHarness(t)
	h := newPoolRoyaltyResolveHandler(poolroyalty.NewResolver(pool))
	future := time.Now().Add(time.Hour)

	mintSeed{req: "h1", contrib: "wsA", requester: "wsB", entry: "E1", status: "held", finalizeAfter: &future}.insert(t, pool)
	mintSeed{req: "h2", contrib: "wsA", requester: "wsB", entry: "E1", status: "held", finalizeAfter: &future}.insert(t, pool)
	mintSeed{req: "h3", contrib: "wsC", requester: "wsD", entry: "E3", status: "held", finalizeAfter: &future}.insert(t, pool)
	mintSeed{req: "s1", contrib: "wsS", requester: "wsT", entry: "ES", layer: "semantic", status: "held", sim: 0.95, finalizeAfter: &future}.insert(t, pool)
	mintSeed{req: "s2", contrib: "wsS", requester: "wsU", entry: "ES", layer: "semantic", status: "held", sim: 0.96, finalizeAfter: &future}.insert(t, pool)

	var vr resolveResponse
	if code := callJSON(t, h, "/v1/admin/pool-royalty/resolve?type=volume&entry_id=E1&contributor=wsA&requester=wsB&window=24h", &vr); code != http.StatusOK {
		t.Fatalf("volume resolve: code %d", code)
	}
	if vr.Label != "tuple_pinned" {
		t.Errorf("volume label=%q want tuple_pinned", vr.Label)
	}
	if len(vr.Candidates) != 2 {
		t.Errorf("volume: %d candidates want 2 (h1,h2)", len(vr.Candidates))
	}

	var sr resolveResponse
	callJSON(t, h, "/v1/admin/pool-royalty/resolve?type=self_dealing&contributor=wsC&requester=wsD&window=24h", &sr)
	if sr.Label != "pair_coarse" {
		t.Errorf("self_dealing label=%q want pair_coarse", sr.Label)
	}
	if len(sr.Candidates) != 1 {
		t.Errorf("self_dealing: %d candidates want 1 (h3)", len(sr.Candidates))
	}

	var simr resolveResponse
	callJSON(t, h, "/v1/admin/pool-royalty/resolve?type=similarity&contributor=wsS&entry_id=ES&sim_min=0.9&sim_max=1.0&window=24h", &simr)
	if simr.Label != "similarity_narrowed" {
		t.Errorf("similarity label=%q want similarity_narrowed", simr.Label)
	}

	if code := callJSON(t, h, "/v1/admin/pool-royalty/resolve?type=bogus", nil); code != http.StatusBadRequest {
		t.Errorf("bad type: code %d want 400", code)
	}
}

// /margin — realized summary = FINAL only (held/revoked excluded), identity holds;
// ?by=… breakdown; bad by → 400.
func TestPoolRoyaltyMargin_Integration(t *testing.T) {
	pool := obsHarness(t)
	h := newPoolRoyaltyMarginHandler(poolroyalty.NewMarginReader(pool))

	mintSeed{req: "f1", contrib: "wsA", requester: "wsB", entry: "E1", status: "final", avoided: 10, minted: 5}.insert(t, pool)
	mintSeed{req: "f2", contrib: "wsC", requester: "wsD", entry: "E2", status: "final", avoided: 6, minted: 3}.insert(t, pool)
	// EXCLUDED from realized margin:
	mintSeed{req: "hX", contrib: "wsA", requester: "wsB", entry: "E3", status: "held", avoided: 100, minted: 50}.insert(t, pool)
	mintSeed{req: "rX", contrib: "wsA", requester: "wsB", entry: "E4", status: "revoked", avoided: 100, minted: 50}.insert(t, pool)

	var resp marginResponse
	if code := callJSON(t, h, "/v1/admin/pool-royalty/margin", &resp); code != http.StatusOK {
		t.Fatalf("margin: code %d", code)
	}
	if resp.Summary.Mints != 2 || resp.Summary.AvoidedCOGSUSD != 16 || resp.Summary.MintedLENS != 8 || resp.Summary.MarginUSD != 8 {
		t.Errorf("summary mints=%d avoided=%v minted=%v margin=%v want 2/16/8/8 (final only)",
			resp.Summary.Mints, resp.Summary.AvoidedCOGSUSD, resp.Summary.MintedLENS, resp.Summary.MarginUSD)
	}
	if resp.Summary.MarginUSD != resp.Summary.AvoidedCOGSUSD-resp.Summary.MintedLENS {
		t.Errorf("margin identity broken: %v != %v − %v", resp.Summary.MarginUSD, resp.Summary.AvoidedCOGSUSD, resp.Summary.MintedLENS)
	}

	var byResp marginResponse
	callJSON(t, h, "/v1/admin/pool-royalty/margin?by=contributor_workspace_id", &byResp)
	if len(byResp.Breakdown) != 2 {
		t.Errorf("breakdown: %d buckets want 2 (wsA, wsC)", len(byResp.Breakdown))
	}

	if code := callJSON(t, h, "/v1/admin/pool-royalty/margin?by=evil", nil); code != http.StatusBadRequest {
		t.Errorf("bad by: code %d want 400", code)
	}
}

// ADVERSARIAL ADMIN-GATE — for ALL THREE endpoints, a non-admin AND an
// unauthenticated request → 401 with NO data leaked in the body.
func TestPoolRoyaltyObs_AdminGate(t *testing.T) {
	pool := obsHarness(t)
	// Seed a row so the data path WOULD return content if the gate ever let it through.
	mintSeed{req: "g1", contrib: "wsA", requester: "wsB", entry: "E1", status: "final", avoided: 10, minted: 5}.insert(t, pool)

	endpoints := []struct {
		name, target string
		h            http.HandlerFunc
	}{
		{"detect", "/v1/admin/pool-royalty/detect", newPoolRoyaltyDetectHandler(poolroyalty.NewDetectorReader(pool, testDetectorThresholds()))},
		{"resolve", "/v1/admin/pool-royalty/resolve?type=volume", newPoolRoyaltyResolveHandler(poolroyalty.NewResolver(pool))},
		{"margin", "/v1/admin/pool-royalty/margin", newPoolRoyaltyMarginHandler(poolroyalty.NewMarginReader(pool))},
	}
	dataKeys := []string{"volume", "bilateral", "similarity", "candidates", "summary", "margin_usd", "avoided_cogs_usd", "minted_lens"}
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
		// Sanity: a real admin DOES pass the gate (200).
		rec := httptest.NewRecorder()
		requireAdmin(fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: true, UserID: "admin1"}}, http.HandlerFunc(ep.h))(rec, httptest.NewRequest(http.MethodGet, ep.target, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s/admin: code %d want 200", ep.name, rec.Code)
		}
	}
}
