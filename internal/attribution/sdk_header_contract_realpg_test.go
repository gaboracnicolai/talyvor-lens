package attribution

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// THE TWO SHIPPED SDKs AND THIS SERVER DISAGREE ABOUT ONE HEADER NAME, AND THE COLUMN IT FILLS IS
// A WHERE PREDICATE IN THREE PLACES.
//
// sdk/python/talyvor_lens/types.py and sdk/typescript/src/types.ts both open with the same claim:
// the HEADER_* constants are "the single source of truth for the wire-level header names. Keep them
// in sync with the Go server-side handlers". Nothing has ever checked it — the SDKs appear in no CI
// workflow, no Makefile target and no Go test.
//
// sdkEmittedHeaders below is not read off those files. It is the set both SDKs were MEASURED to
// emit, by executing them with every attribution field set:
//
//	python -c "from talyvor_lens.middleware import inject_lens_headers; ..."
//	npx jest  (injectLensHeaders, Object.keys)
//
// Both produce the identical ten. The server stores repo_name from X-Talyvor-Repo — a spelling
// NEITHER SDK emits, and which no shipped Talyvor client anywhere in the estate emits.
//
// ⚠ THE POSITIVE CONTROL IS THE POINT OF THIS FILE. "ByRepo is empty" is also what a harness that
// inserted no rows produces. Every zero here is asserted beside a non-zero the SAME harness
// produced from the SAME request — ByBranch and ByPR are populated by the SDK's own headers, and
// the X-Talyvor-Repo spelling lands a row. A zero without that control is not evidence.
var sdkEmittedHeaders = map[string]string{
	"Authorization":        "Bearer tlv_x",
	"X-Talyvor-Workspace":  "", // filled per test — see w639Workspaces
	"X-Talyvor-Team":       "core",
	"X-Talyvor-Feature":    "search",
	"X-Talyvor-Session":    "sess-1",
	"X-Talyvor-Agent":      "ranger",
	"X-Talyvor-Branch":     "feat/login",
	"X-Talyvor-PR":         "42",
	"X-Talyvor-Commit":     "abc123",
	"X-Talyvor-Repository": "acme/widgets",
}

const (
	repoNm = "acme/widgets"
	// Its own database, created beside the DSN's — never the shared lens_test.
	w639DB = "lens_w639_sdk_headers"
)

// request_attribution is APPEND-ONLY — migration U14 installs a trigger that blocks DELETE, so a
// test cannot tidy up after itself. Each test therefore gets its own workspace pair and asserts
// only within it. (Discovered by the harness failing on its own cleanup, not by reading the DDL.)
var w639Seq int64

func w639Workspaces(t *testing.T) (sdk, ctl string) {
	t.Helper()
	w639Seq++
	n := time.Now().UnixNano() + w639Seq
	return fmt.Sprintf("ws-w639-sdk-%d", n), fmt.Sprintf("ws-w639-ctl-%d", n)
}

// sdkHeaders returns the measured SDK header set bound to a workspace.
func sdkHeaders(ws string) map[string]string {
	h := make(map[string]string, len(sdkEmittedHeaders))
	for k, v := range sdkEmittedHeaders {
		h[k] = v
	}
	h["X-Talyvor-Workspace"] = ws
	return h
}

func w639Store(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG SDK header contract test")
	}
	ctx := context.Background()

	// ⚠ THIS TEST MIGRATES, AND IT MUST NOT MIGRATE THE SHARED GATED-TEST DATABASE. CI's own comment
	// says lens_test is never migrated — every gated test builds its own fixture tables — and it
	// keeps `lens_migrate_check` separate for exactly that reason. Running migrations against
	// lens_test would install the U14 append-only trigger on request_attribution, and
	// internal/api's TestSpendByRequest DELETEs from that table in its cleanup. So this test does
	// what CI does: its own database, the real migration chain, nobody else's schema touched.
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+w639DB); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		admin.Close()
		t.Skipf("cannot create an isolated database (%v) — refusing to migrate the shared %s", err, url)
	}
	admin.Close()

	pool, err := pgxpool.New(ctx, swapDatabase(t, url, w639DB))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, conn.Conn(), migrations.FS); err != nil {
		conn.Release()
		t.Fatalf("migrate: %v", err)
	}
	conn.Release()
	return NewStore(pool)
}

// swapDatabase rewrites the database name in a Postgres DSN, keeping every other component.
func swapDatabase(t *testing.T, dsn, db string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	u.Path = "/" + db
	return u.String()
}

// requestFrom builds a real *http.Request carrying exactly hdrs. The row under test must be written
// by the real ExtractFromRequest from a real request — a hand-built AttributionContext would prove
// nothing about the wire.
func requestFrom(t *testing.T, hdrs map[string]string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdrs {
		r.Header.Set(k, v)
	}
	return r
}

func record(t *testing.T, s *Store, r *http.Request) AttributionContext {
	t.Helper()
	attr := ExtractFromRequest(r)
	if err := s.Record(context.Background(), attr, 10, 20, 0.05, "gpt-4o", "openai", 100*time.Millisecond); err != nil {
		t.Fatalf("record: %v", err)
	}
	return attr
}

// A request carrying the header set the SDKs were measured to emit must land its repository on
// request_attribution.repo_name. The SDK README documents X-Talyvor-Repository as the public header
// and the `repository` kwarg as an "owner/name repo identifier"; a value the client sends and the
// server drops is a captured field with no join key.
func TestSDKEmittedRepositoryHeaderReachesRepoName(t *testing.T) {
	s := w639Store(t)
	ctx := context.Background()

	wsSDK, wsCtl := w639Workspaces(t)
	attr := record(t, s, requestFrom(t, sdkHeaders(wsSDK)))

	// POSITIVE CONTROL, same harness, same table: the other spelling lands. If this row is missing
	// the harness is broken and the assertion below means nothing.
	ctl := map[string]string{"X-Talyvor-Workspace": wsCtl, "X-Talyvor-Branch": "feat/login", "X-Talyvor-Repo": repoNm}
	record(t, s, requestFrom(t, ctl))
	var ctlRepo string
	if err := s.pool.QueryRow(ctx,
		`SELECT repo_name FROM request_attribution WHERE workspace_id=$1`, wsCtl).Scan(&ctlRepo); err != nil {
		t.Fatalf("control read: %v", err)
	}
	if ctlRepo != repoNm {
		t.Fatalf("HARNESS IS BLIND: the X-Talyvor-Repo control did not land either (repo_name=%q). "+
			"Nothing below this line is evidence.", ctlRepo)
	}

	var gotRepo, gotBranch, gotPR string
	if err := s.pool.QueryRow(ctx,
		`SELECT repo_name, branch, pr_number FROM request_attribution WHERE workspace_id=$1`, wsSDK).
		Scan(&gotRepo, &gotBranch, &gotPR); err != nil {
		t.Fatalf("read: %v", err)
	}
	// The same request's branch and PR DID land — so the row exists and the writer works.
	if gotBranch != "feat/login" || gotPR != "42" {
		t.Fatalf("harness: SDK row did not carry branch/pr (branch=%q pr=%q)", gotBranch, gotPR)
	}
	if gotRepo != repoNm {
		t.Errorf("SDK-emitted X-Talyvor-Repository=%q did not reach request_attribution.repo_name: got %q.\n"+
			"ExtractFromRequest reads X-Talyvor-Repo; both shipped SDKs emit X-Talyvor-Repository and no\n"+
			"shipped Talyvor client anywhere emits X-Talyvor-Repo. In-memory Git.RepoName=%q.",
			repoNm, gotRepo, attr.Git.RepoName)
	}
}

// The dashboard's Git Attribution tab. ByBranch and ByPR are populated from the SAME SDK request
// that leaves ByRepo empty — that pairing is what makes the zero structural rather than absent.
func TestGitAttributionRollupIsNotStructurallyZeroForSDKTraffic(t *testing.T) {
	s := w639Store(t)
	wsSDK, _ := w639Workspaces(t)
	record(t, s, requestFrom(t, sdkHeaders(wsSDK)))

	sum, err := s.GetSummary(context.Background(), wsSDK, 30)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(sum.ByBranch) == 0 || len(sum.ByPR) == 0 {
		t.Fatalf("HARNESS IS BLIND: ByBranch=%d ByPR=%d from the SDK request — the rollup saw no rows at "+
			"all, so an empty ByRepo below would prove nothing.", len(sum.ByBranch), len(sum.ByPR))
	}
	if len(sum.ByRepo) == 0 {
		t.Errorf("Summary.ByRepo is EMPTY for a request carrying the SDK's own X-Talyvor-Repository, while "+
			"ByBranch=%d and ByPR=%d from the same request are populated. summaryByRepoSQL filters "+
			"repo_name != '' and repo_name can only ever be '' for shipped-client traffic — a structural "+
			"zero presented on the dashboard as a measurement.", len(sum.ByBranch), len(sum.ByPR))
	}
}

// GET /v1/attribution/branch 400s without a non-empty ?repository, then matches it against
// repo_name. GET /v1/attribution/top does the same. Both are unreachable for shipped-client traffic.
func TestBranchSpendLookupIsReachableForSDKTraffic(t *testing.T) {
	s := w639Store(t)
	ctx := context.Background()
	wsSDK, wsCtl := w639Workspaces(t)
	record(t, s, requestFrom(t, sdkHeaders(wsSDK)))

	// POSITIVE CONTROL: the same lookup, same code path, on a row written with X-Talyvor-Repo.
	ctl := map[string]string{"X-Talyvor-Workspace": wsCtl, "X-Talyvor-Branch": "feat/login", "X-Talyvor-Repo": repoNm}
	record(t, s, requestFrom(t, ctl))
	ctlGot, err := s.GetBranchSpendForWorkspace(ctx, wsCtl, "feat/login", repoNm)
	if err != nil {
		t.Fatalf("control lookup: %v", err)
	}
	if ctlGot == nil {
		t.Fatal("HARNESS IS BLIND: the X-Talyvor-Repo control lookup found nothing either. " +
			"Nothing below this line is evidence.")
	}

	got, err := s.GetBranchSpendForWorkspace(ctx, wsSDK, "feat/login", repoNm)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got == nil {
		t.Errorf("GetBranchSpendForWorkspace(%q, %q) found nothing for a request the SDK itself tagged "+
			"with repository=%q — the identical control lookup on an X-Talyvor-Repo row returned a row. "+
			"GET /v1/attribution/branch answers 404 for every shipped client, for every repository value, "+
			"and the empty value it would need is rejected 400.", "feat/login", repoNm, repoNm)
	}
	top, err := s.GetTopBranchesForWorkspace(ctx, wsSDK, repoNm, 10)
	if err != nil {
		t.Fatalf("top: %v", err)
	}
	if len(top) == 0 {
		t.Errorf("GetTopBranchesForWorkspace returned 0 rows for repository=%q — GET /v1/attribution/top "+
			"answers [] for every shipped client.", repoNm)
	}
}
