package modelwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// B10.5 — against real Postgres, with a stub provider list standing in for Anthropic's /v1/models.

func discoveryDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG model discovery test")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_discovery_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = ac.Close(ctx)
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	mc, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		_ = mc.Close(ctx)
		t.Fatalf("migrate: %v", err)
	}
	_ = mc.Close(ctx)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if c, err := pgx.Connect(context.Background(), admin); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	return pool
}

// listServer serves a model list the test can change between polls, as a provider's changes overnight.
type listServer struct {
	mu  sync.Mutex
	ids []string
	srv *httptest.Server
}

func newListServer(t *testing.T, ids []string) *listServer {
	t.Helper()
	l := &listServer{ids: ids}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		data := make([]map[string]string, 0, len(l.ids))
		for _, id := range l.ids {
			data = append(data, map[string]string{"id": id})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(l.srv.Close)
	return l
}

func (l *listServer) set(ids []string) { l.mu.Lock(); l.ids = ids; l.mu.Unlock() }

// anthropicCatalogIDs is every Anthropic model the catalog has, minus skip — "the provider lists all of them".
func anthropicCatalogIDs(skip string) []string {
	var ids []string
	for _, m := range catalog.All() {
		if m.Provider == "anthropic" && m.ID != skip {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

func discoveryWatcher(t *testing.T, url string) *Watcher {
	w := watcherAgainst(url, nil)
	w.retired, w.applied = map[string]catalog.Model{}, map[string]bool{}
	w.SetStore(NewStore(discoveryDB(t)))
	return w
}

// offered is the suite chat picker's rule: priced for output and not deprecated.
func offered(id string) bool {
	m, ok := catalog.Get(id)
	return ok && !m.Deprecated && m.OutputPer1M > 0
}

// DONE: a model the provider adds tomorrow appears as "needs a price" with no code change — not in
// the catalog, so neither offered nor billed at zero — and is offered as soon as a person prices it.
func TestDiscovery_ANewModelNeedsAPriceThenIsOfferedOnceItIsPriced(t *testing.T) {
	const tomorrow = "claude-b105-discovered-9"
	list := newListServer(t, anthropicCatalogIDs(""))
	w := discoveryWatcher(t, list.srv.URL)
	ctx := context.Background()

	w.checkAndAlert(ctx) // today
	list.set(append(anthropicCatalogIDs(""), tomorrow))
	w.checkAndAlert(ctx) // tomorrow's poll

	found, err := w.Discovered(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != tomorrow || found[0].Provider != "anthropic" || !found[0].NeedsPrice {
		t.Fatalf("discovered = %+v, want exactly %s from anthropic, needing a price", found, tomorrow)
	}
	if _, inCatalog := catalog.Get(tomorrow); inCatalog {
		t.Fatalf("%s entered the pricing catalog unpriced — it would be billed at zero", tomorrow)
	}

	m, err := w.ConfirmPrice(ctx, tomorrow, 3, 15, "https://platform.claude.com/docs/en/about-claude/pricing")
	if err != nil {
		t.Fatalf("confirm price: %v", err)
	}
	if m.InputPer1M != 3 || m.OutputPer1M != 15 || !offered(tomorrow) {
		t.Errorf("after pricing, %s = %+v; want 3/15 and offered in the chat picker", tomorrow, m)
	}
	if found, _ := w.Discovered(ctx); len(found) != 0 {
		t.Errorf("still waiting for a price after it was priced: %+v", found)
	}
}

// A model the provider stops listing leaves the picker (its price stays, so a straggling API call is
// still billed right), and comes back if the provider lists it again.
func TestDiscovery_AModelTheProviderNoLongerListsLeavesThePickerAndCanComeBack(t *testing.T) {
	const gone = "claude-opus-4-1"
	before, ok := catalog.Get(gone)
	if !ok || !offered(gone) {
		t.Fatalf("%s must be an offered catalog model for this test to mean anything", gone)
	}
	t.Cleanup(func() { catalog.Override(before) })
	list := newListServer(t, anthropicCatalogIDs(gone))
	w := discoveryWatcher(t, list.srv.URL)

	w.checkAndAlert(context.Background())
	if offered(gone) {
		t.Fatalf("%s is still offered after its provider stopped listing it", gone)
	}
	if in, out, ok := catalog.Price(gone); !ok || in != before.InputPer1M || out != before.OutputPer1M {
		t.Errorf("retiring %s changed its price to %v/%v (ok=%v)", gone, in, out, ok)
	}

	list.set(anthropicCatalogIDs(""))
	w.checkAndAlert(context.Background())
	if !offered(gone) {
		t.Errorf("%s is listed again but still not offered", gone)
	}
}

// A price is a person's, with its source; the seed's price is never shadowed by one.
func TestDiscovery_APriceNeedsItsSourceAndNeverOverridesTheSeed(t *testing.T) {
	list := newListServer(t, append(anthropicCatalogIDs(""), "claude-b105-unsourced-9"))
	w := discoveryWatcher(t, list.srv.URL)
	ctx := context.Background()
	w.checkAndAlert(ctx)

	if _, err := w.ConfirmPrice(ctx, "claude-b105-unsourced-9", 3, 15, ""); err == nil || offered("claude-b105-unsourced-9") {
		t.Errorf("a price with no source URL was accepted (err=%v)", err)
	}
	seeded := anthropicCatalogIDs("")[0]
	was, _ := catalog.Get(seeded)
	if _, err := w.ConfirmPrice(ctx, seeded, 0.01, 0.01, "https://example.com"); err != ErrAlreadyPriced {
		t.Errorf("pricing seeded %s: err = %v, want ErrAlreadyPriced", seeded, err)
	}
	if now, _ := catalog.Get(seeded); now.InputPer1M != was.InputPer1M || now.OutputPer1M != was.OutputPer1M {
		t.Errorf("seeded %s repriced to %v/%v", seeded, now.InputPer1M, now.OutputPer1M)
	}
}
