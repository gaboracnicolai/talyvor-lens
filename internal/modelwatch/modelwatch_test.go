package modelwatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeNotifier records what would have been delivered, so a test can assert a PERSON was told rather
// than that a function returned.
type fakeNotifier struct {
	calls    int
	subjects []string
	bodies   []string
	err      error
}

func (f *fakeNotifier) Notify(_ context.Context, subject, body string) error {
	f.calls++
	f.subjects = append(f.subjects, subject)
	f.bodies = append(f.bodies, body)
	return f.err
}
func (f *fakeNotifier) Describe() string { return "fake" }

// modelsServer serves an Anthropic/OpenAI-shaped model list.
func modelsServer(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var data []map[string]string
		for _, id := range ids {
			data = append(data, map[string]string{"id": id})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

// watcherAgainst points a Watcher at a stub endpoint instead of the real provider.
func watcherAgainst(url string, n Notifier) *Watcher {
	return &Watcher{
		client:   http.DefaultClient,
		notifier: n,
		alerted:  map[string]bool{},
		providers: []provider{{
			name: "anthropic", url: url, key: "k",
			setAuth: func(r *http.Request, k string) { r.Header.Set("x-api-key", k) },
		}},
	}
}

// ⭐ THE POINT OF THE WHOLE PACKAGE: a model the provider serves and the catalog cannot price is
// reported, and one it CAN price is not.
func TestCheck_ReportsOnlyWhatTheCatalogCannotPrice(t *testing.T) {
	// claude-opus-5 is in the catalog as of this PR, so it must NOT be reported; the synthetic id must.
	srv := modelsServer(t, "claude-opus-5", "claude-a-model-that-does-not-exist-v9")
	defer srv.Close()

	findings, errs := watcherAgainst(srv.URL, nil).Check(context.Background())
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want exactly 1: %+v", len(findings), findings)
	}
	if findings[0].ModelID != "claude-a-model-that-does-not-exist-v9" {
		t.Errorf("reported the wrong model: %q", findings[0].ModelID)
	}
	// The finding must carry what Lens would currently bill, or the alert is unactionable.
	if findings[0].FallbackOutputPer1M <= 0 {
		t.Errorf("finding has no fallback rate (%v) — the alert would not say what is being billed",
			findings[0].FallbackOutputPer1M)
	}
}

// A priced model must never alert. This is the noise guard: an hourly alert about a model that is
// correctly configured trains an operator to ignore the channel, which is how the next real one is
// missed.
func TestCheck_KnownModelsProduceNoAlert(t *testing.T) {
	srv := modelsServer(t, "claude-opus-5", "claude-sonnet-5", "claude-opus-4-8")
	defer srv.Close()
	f := &fakeNotifier{}
	watcherAgainst(srv.URL, f).checkAndAlert(context.Background())
	if f.calls != 0 {
		t.Errorf("alerted %d time(s) on a fully-priced model list: %v", f.calls, f.subjects)
	}
}

// The alert must reach the notifier, name the model, AND carry the remedy.
func TestCheckAndAlert_DeliversAnActionableAlert(t *testing.T) {
	srv := modelsServer(t, "claude-brand-new-thing-v9")
	defer srv.Close()
	f := &fakeNotifier{}
	watcherAgainst(srv.URL, f).checkAndAlert(context.Background())

	if f.calls != 1 {
		t.Fatalf("notifier called %d times, want 1 — the alert did not reach the sink", f.calls)
	}
	body := f.bodies[0]
	for _, want := range []string{
		"claude-brand-new-thing-v9",       // which model
		"LENS_MODEL_CATALOG_OVERRIDES",    // the immediate remedy
		"platform.claude.com",             // where the real rate lives
		"DO NOT let an automated process", // the never-scrape rule travels WITH the alert
	} {
		if !strings.Contains(body, want) {
			t.Errorf("alert body is missing %q — an alert without the action makes the reader rediscover it\n%s", want, body)
		}
	}
}

// Re-alerting the same model every hour is how a channel becomes noise; but a model must be re-alerted
// if delivery FAILED, or a transient webhook outage silently loses the only notification.
func TestCheckAndAlert_DedupsDeliveredButRetriesFailed(t *testing.T) {
	srv := modelsServer(t, "claude-brand-new-thing-v9")
	defer srv.Close()

	t.Run("delivered once, then quiet", func(t *testing.T) {
		f := &fakeNotifier{}
		w := watcherAgainst(srv.URL, f)
		w.checkAndAlert(context.Background())
		w.checkAndAlert(context.Background())
		w.checkAndAlert(context.Background())
		if f.calls != 1 {
			t.Errorf("notifier called %d times across 3 polls, want 1", f.calls)
		}
	})

	t.Run("delivery failed, so it retries", func(t *testing.T) {
		f := &fakeNotifier{err: context.DeadlineExceeded}
		w := watcherAgainst(srv.URL, f)
		w.checkAndAlert(context.Background())
		w.checkAndAlert(context.Background())
		if f.calls != 2 {
			t.Errorf("notifier called %d times after a FAILED delivery, want 2 — a dropped alert must be "+
				"retried, not recorded as sent", f.calls)
		}
	})
}

// ⚠ An empty 200 must be a FAILED POLL, never "no drift". This is the absence-read-as-a-value bug that
// this whole change exists to remove, in its newest possible location.
func TestListModels_EmptyResponseIsAFailureNotAllClear(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	_, errs := watcherAgainst(srv.URL, nil).Check(context.Background())
	if len(errs) != 1 {
		t.Fatalf("an empty model list produced %d errors, want 1 — it was read as 'no new models'", len(errs))
	}
	if !strings.Contains(errs[0].Error(), "not as 'no drift'") {
		t.Errorf("error does not explain the distinction: %v", errs[0])
	}
}

// One unreachable provider must not suppress another's findings.
func TestCheck_OneProviderDownDoesNotHideTheOther(t *testing.T) {
	good := modelsServer(t, "claude-brand-new-thing-v9")
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	w := watcherAgainst(good.URL, nil)
	w.providers = append(w.providers, provider{
		name: "openai", url: bad.URL, key: "k",
		setAuth: func(r *http.Request, k string) { r.Header.Set("Authorization", "Bearer "+k) },
	})
	findings, errs := w.Check(context.Background())
	if len(findings) != 1 {
		t.Errorf("a dead provider suppressed the live one's findings: %+v", findings)
	}
	if len(errs) != 1 {
		t.Errorf("the dead provider did not report an error: %v", errs)
	}
}

// A provider with no key is skipped rather than polled with an empty credential.
func TestNew_SkipsProvidersWithoutAKey(t *testing.T) {
	if got := len(New("", "", nil).providers); got != 0 {
		t.Errorf("no keys configured but %d providers registered", got)
	}
	if got := len(New("anthropic-key", "", nil).providers); got != 1 {
		t.Errorf("one key configured but %d providers registered", got)
	}
	if got := len(New("a", "b", nil).providers); got != 2 {
		t.Errorf("two keys configured but %d providers registered", got)
	}
}

// ⚠ A TRUNCATED list must be a failed poll, never a false all-clear. Anthropic's /v1/models paginates
// and defaults to 20 items; there are already ~20 Claude models, so a new one can land on page 2. If a
// partial page were diffed, every unread id would count as "nothing new" and the poller would report
// all-clear while blind — the absence-read-as-a-value bug in its newest hiding place.
func TestListModels_TruncatedListIsAFailureNotAllClear(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Every id present IS priced, so a naive implementation reports zero findings and no error.
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5"}],"has_more":true}`))
	}))
	defer srv.Close()

	findings, errs := watcherAgainst(srv.URL, nil).Check(context.Background())
	if len(findings) != 0 {
		t.Fatalf("unexpected findings: %+v", findings)
	}
	if len(errs) != 1 {
		t.Fatalf("a truncated list produced %d errors, want 1 — it was accepted as a complete poll", len(errs))
	}
	if !strings.Contains(errs[0].Error(), "TRUNCATED") {
		t.Errorf("error does not name truncation: %v", errs[0])
	}
}

// The real Anthropic endpoint must be requested with a page limit, or the default of 20 silently
// truncates. Pinned because it is a one-character deletion away from being a blind check that passes.
func TestNew_AnthropicRequestsALargePage(t *testing.T) {
	w := New("key", "", nil)
	if len(w.providers) != 1 {
		t.Fatal("expected one provider")
	}
	if !strings.Contains(w.providers[0].url, "limit=") {
		t.Errorf("anthropic model-list URL has no page limit (%q) — it would default to 20 items and "+
			"silently miss models on later pages", w.providers[0].url)
	}
}
