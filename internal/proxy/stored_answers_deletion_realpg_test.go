package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/migrations"
)

// B21.3 — A WORKSPACE DELETES ITS STORED ANSWERS, OR ASKS TALYVOR TO DELETE EVERYTHING.
//
// One migrated schema holds the answers AND the money, so "the ledger rows remain" is asserted on
// the same database the deletion ran against. Answers are asked through the real chat handler
// (HandleAnthropic, streamed, a session key); deletion goes through the real storedanswers routes.

var storedAnswersMigrateOnce sync.Once

func storedAnswersDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG stored-answers deletion test")
	}
	const schema = "proxy_storedanswers_realpg"
	ctx := context.Background()
	storedAnswersMigrateOnce.Do(func() {
		cfg, err := pgx.ParseConfig(url)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		cfg.RuntimeParams["search_path"] = schema + ",public"
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer conn.Close(ctx)
		for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
			if _, err := conn.Exec(ctx, ddl); err != nil {
				t.Fatalf("reset schema: %v", err)
			}
		}
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	})
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("pool cfg: %v", err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	db, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(db.Close)
	for _, q := range []string{
		`DELETE FROM prompt_embeddings`, `DELETE FROM stored_answer_deletions`, `DELETE FROM deletion_requests`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatalf("reset: %v", err)
		}
	}
	return db
}

type storedAnswersRig struct {
	p      *Proxy
	db     *pgxpool.Pool
	wsm    *workspace.Manager
	calls  *int64
	routes http.Handler
}

// newStoredAnswersRig is chatCacheProxy on one migrated database: pooling on, wsSAA and wsSAB opted
// in, funded and earn-verified, the production minter armed, the exact cache in miniredis and the
// semantic cache in Postgres — plus the B21.3 routes on the same Postgres and Redis.
func newStoredAnswersRig(t *testing.T) *storedAnswersRig {
	t.Helper()
	ctx := context.Background()
	db := storedAnswersDB(t)
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	p.router = nil
	store := economy.NewDualTokenStore(nil, db, nil)
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	p.SetLXCSpendSink(store, func() bool { return false })
	p.SetLXCGate(store, func() bool { return false })
	p.SetPoolConsumerDiscount(0.30)

	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatSSE("The capital of the UK is London.", true))
	}))
	t.Cleanup(up.Close)
	p.anthropicURL = up.URL

	exact, mr := newExactCacheForTest(t)
	p.exact = exact
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	sem := cache.NewSemanticCache(db, fpEmbedder{}, 0.98, 0)
	sem.SetPairVerifier(yesVerifier{})
	p.semantic, p.embedder = sem, fpEmbedder{}

	wsm := p.workspaceManager
	for _, id := range []string{"wsSAA", "wsSAB"} {
		if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{ID: id, Name: id, Active: true, LoggingPolicy: workspace.LoggingMetadata}); err != nil {
			t.Fatal(err)
		}
		if err := wsm.SetCachePoolable(ctx, id, true); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified) VALUES ($1, $1, $1, true)
			ON CONFLICT (id) DO UPDATE SET earn_verified = true`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, $2, $2)
			ON CONFLICT (workspace_id) DO UPDATE SET balance = EXCLUDED.balance, cash_backed_ulxc = EXCLUDED.cash_backed_ulxc`,
			id, costWireFunded); err != nil {
			t.Fatal(err)
		}
	}
	on := true
	p.SetPoolGate(cache_pooling.New(func() bool { return on }, wsm.GetCachePoolable))
	ledger := mining.NewLedgerStore(db)
	ledger.SetMintVerifier(earnverify.New(false))
	p.SetRoyaltyMinter(poolroyalty.NewMinter(db, ledger, 0.5, func() bool { return true }))

	answers := storedanswers.New(db, rdb)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // the credential the test names, as AuthMiddleware would
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			actx := &auth.AuthContext{WorkspaceID: req.Header.Get("X-Test-Workspace"), AuthMethod: auth.MethodJWT}
			switch req.Header.Get("X-Test-Credential") {
			case "owner":
				actx.Scopes = []string{auth.ScopeAnalytics, auth.ScopeKeys}
			case "chat":
				actx.AuthMethod, actx.Scopes = auth.MethodSessionKey, []string{auth.ScopeProxy}
			case "operator":
				actx.AuthMethod, actx.IsAdmin = auth.MethodGlobalKey, true
			}
			next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), actx)))
		})
	})
	r.Get("/v1/workspaces/{wsID}/stored-answers", storedanswers.CountsHandler(answers))
	r.Delete("/v1/workspaces/{wsID}/stored-answers", storedanswers.DeleteHandler(answers, wsm))
	r.Post("/v1/workspaces/{wsID}/deletion-requests", storedanswers.FileRequestHandler(answers))
	r.Get("/v1/workspaces/{wsID}/deletion-requests", storedanswers.RequestStatusHandler(answers))
	r.Get("/v1/admin/deletion-requests", storedanswers.AdminListHandler(answers))
	r.Post("/v1/admin/deletion-requests/{id}/complete", storedanswers.AdminCompleteHandler(answers))
	return &storedAnswersRig{p: p, db: db, wsm: wsm, calls: &calls, routes: r}
}

func (g *storedAnswersRig) call(t *testing.T, method, path, ws, credential, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Test-Workspace", ws)
	req.Header.Set("X-Test-Credential", credential)
	w := httptest.NewRecorder()
	g.routes.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (g *storedAnswersRig) count(t *testing.T, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := g.db.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// asked sends the chat's question for ws and reports whether the model answered it (true) or the
// cache did (false).
func (g *storedAnswersRig) asked(t *testing.T, ws string) (askedModel bool) {
	t.Helper()
	before := atomic.LoadInt64(g.calls)
	chatAsk(t, g.p, ws)
	return atomic.LoadInt64(g.calls) > before
}

func (g *storedAnswersRig) royaltyHeld(t *testing.T, ws string) int64 {
	return g.count(t, `SELECT COALESCE(sum(amount), 0) FROM lens_token_ledger WHERE workspace_id = $1 AND type = $2`,
		ws, mining.TypePoolRoyaltyHeld)
}

func TestStoredAnswers_SharedBeforeSwitchingOffStillServesAndEarns_UntilDeleted(t *testing.T) {
	g := newStoredAnswersRig(t)
	ctx := context.Background()

	if !g.asked(t, "wsSAA") {
		t.Fatal("the first question must reach the model")
	}
	if err := g.wsm.SetCachePoolable(ctx, "wsSAA", false); err != nil { // wsSAA switches sharing off
		t.Fatal(err)
	}

	// Shared before switching off: still served to another workspace, and still earns.
	if g.asked(t, "wsSAB") {
		t.Fatal("wsSAA's answer, shared before it switched sharing off, was not served to wsSAB")
	}
	earned := g.royaltyHeld(t, "wsSAA")
	if earned <= 0 {
		t.Fatalf("wsSAA earned %d µLENS from the serve; want > 0", earned)
	}

	// Refusals: not the owner, and a wrong name.
	if code, _ := g.call(t, http.MethodDelete, "/v1/workspaces/wsSAA/stored-answers", "wsSAA", "chat",
		`{"scope":"shared","confirm":"wsSAA"}`); code != http.StatusForbidden {
		t.Errorf("the chat's session key deleted stored answers: status %d, want 403", code)
	}
	if code, _ := g.call(t, http.MethodDelete, "/v1/workspaces/wsSAA/stored-answers", "wsSAA", "owner",
		`{"scope":"shared","confirm":"wsSAB"}`); code != http.StatusBadRequest {
		t.Errorf("a wrong workspace name was accepted: status %d, want 400", code)
	}
	if n := g.count(t, `SELECT count(*) FROM prompt_embeddings WHERE is_poolable AND contributor_workspace_id = 'wsSAA'`); n == 0 {
		t.Fatal("a refused deletion deleted the shared answer")
	}

	// "shared": the shared answer is gone and no longer served; the private one still serves.
	code, out := g.call(t, http.MethodDelete, "/v1/workspaces/wsSAA/stored-answers", "wsSAA", "owner",
		`{"scope":"shared","confirm":"wsSAA"}`)
	if code != http.StatusOK {
		t.Fatalf("delete shared: status %d %v", code, out)
	}
	if d, _ := out["deleted"].(map[string]any); d["shared_answers"] != float64(1) || d["private_answers"] != float64(0) {
		t.Errorf("delete shared returned %v; want 1 shared and 0 private answers", out["deleted"])
	}
	if n := g.count(t, `SELECT count(*) FROM prompt_embeddings WHERE is_poolable AND contributor_workspace_id = 'wsSAA'`); n != 0 {
		t.Errorf("%d shared rows of wsSAA remain after deleting shared answers", n)
	}
	if !g.asked(t, "wsSAB") {
		t.Error("wsSAB was served wsSAA's deleted shared answer")
	}
	if g.asked(t, "wsSAA") {
		t.Error("wsSAA's own private answer stopped serving after deleting only its shared answers")
	}
	if got := g.royaltyHeld(t, "wsSAA"); got != earned {
		t.Errorf("wsSAA's earnings moved from %d to %d on deletion; earnings are never clawed back here", earned, got)
	}

	// "all": its private answer is gone too, and nothing of wsSAA's is served.
	if code, out = g.call(t, http.MethodDelete, "/v1/workspaces/wsSAA/stored-answers", "wsSAA", "owner",
		`{"scope":"all","confirm":"wsSAA"}`); code != http.StatusOK {
		t.Fatalf("delete all: status %d %v", code, out)
	}
	if n := g.count(t, `SELECT count(*) FROM prompt_embeddings WHERE workspace_id = 'wsSAA' OR contributor_workspace_id = 'wsSAA'`); n != 0 {
		t.Errorf("%d rows of wsSAA remain after deleting everything", n)
	}
	if !g.asked(t, "wsSAA") {
		t.Error("wsSAA was served an answer after deleting everything it stored")
	}
	if n := g.count(t, `SELECT count(*) FROM stored_answer_deletions WHERE workspace_id = 'wsSAA'`); n != 2 {
		t.Errorf("audit log has %d deletions for wsSAA, want 2 (shared, then all)", n)
	}
}

func TestStoredAnswers_ADeletionRequestIsCompletedByTheOperator_AndTheLedgerStays(t *testing.T) {
	g := newStoredAnswersRig(t)
	g.asked(t, "wsSAA")
	if g.asked(t, "wsSAB") {
		t.Fatal("setup: wsSAB was not served wsSAA's shared answer")
	}
	ledgerRows := `SELECT (SELECT count(*) FROM lxc_ledger) + (SELECT count(*) FROM lens_token_ledger) + (SELECT count(*) FROM pool_royalty_mints)`
	before := g.count(t, ledgerRows)
	if before == 0 {
		t.Fatal("setup: no ledger rows to keep — the assertion below would prove nothing")
	}

	if code, _ := g.call(t, http.MethodPost, "/v1/workspaces/wsSAA/deletion-requests", "wsSAA", "chat", `{}`); code != http.StatusForbidden {
		t.Errorf("the chat's session key filed a deletion request: status %d, want 403", code)
	}
	code, out := g.call(t, http.MethodPost, "/v1/workspaces/wsSAA/deletion-requests", "wsSAA", "owner", `{"note":"delete my data"}`)
	if code != http.StatusCreated || out["status"] != "requested" {
		t.Fatalf("file request: status %d %v; want 201 requested", code, out)
	}
	id := int64(out["id"].(float64))

	_, list := g.call(t, http.MethodGet, "/v1/admin/deletion-requests", "", "operator", "")
	if reqs, _ := list["requests"].([]any); len(reqs) != 1 {
		t.Fatalf("operator queue = %v; want the one open request", list)
	}
	code, out = g.call(t, http.MethodPost, "/v1/admin/deletion-requests/"+itoa(id)+"/complete", "", "operator", "")
	if code != http.StatusOK {
		t.Fatalf("complete: status %d %v", code, out)
	}
	if req, _ := out["request"].(map[string]any); req["status"] != "done" || req["completed_at"] == nil {
		t.Errorf("completed request = %v; want status done with a completion time", out["request"])
	}
	if !strings.Contains(out["kept"].(string), "ledger records") {
		t.Errorf("the completion does not say the ledger is kept: %v", out["kept"])
	}
	if after := g.count(t, ledgerRows); after != before {
		t.Errorf("ledger rows went from %d to %d on completion; billing and ledger records must be kept", before, after)
	}

	_, status := g.call(t, http.MethodGet, "/v1/workspaces/wsSAA/deletion-requests", "wsSAA", "owner", "")
	if reqs, _ := status["requests"].([]any); len(reqs) != 1 || reqs[0].(map[string]any)["status"] != "done" {
		t.Errorf("the requester sees %v; want its one request, done", status)
	}
	if n := g.count(t, `SELECT count(*) FROM prompt_embeddings WHERE workspace_id = 'wsSAA' OR contributor_workspace_id = 'wsSAA'`); n != 0 {
		t.Errorf("%d rows of wsSAA remain after its deletion request was completed", n)
	}
	if !g.asked(t, "wsSAA") {
		t.Error("wsSAA was served an answer after its deletion request was completed")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
