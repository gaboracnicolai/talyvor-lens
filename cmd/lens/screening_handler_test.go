package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/screening"
)

// B30.6 — the operator reads the lists and the cases, and an admin releases or refuses a held one, named by
// X-Talyvor-Operator. The store and screener are faked: internal/screening's real-PG tests cover them.

type fakeScreening struct {
	cases   map[string]screening.Case
	decided []string
}

func (f *fakeScreening) Lists(context.Context) ([]screening.ListStatus, error) {
	return []screening.ListStatus{{List: "UK", Entries: 15337}, {List: "OFAC", Entries: 39083, Stale: true, LastError: "download: 503"}}, nil
}
func (f *fakeScreening) ThresholdBPS() int { return 9000 }
func (f *fakeScreening) Cases(_ context.Context, status string, _ int) ([]screening.Case, error) {
	out := []screening.Case{}
	for _, c := range f.cases {
		if status == "" || c.Status == status {
			out = append(out, c)
		}
	}
	return out, nil
}
func (f *fakeScreening) Decide(_ context.Context, id string, release bool, by, note string) (screening.Case, error) {
	c, ok := f.cases[id]
	switch {
	case !ok:
		return screening.Case{}, screening.ErrCaseNotFound
	case c.Status != screening.CaseHeld:
		return screening.Case{}, screening.ErrCaseDecided
	}
	c.Status, c.DecidedBy, c.DecisionNote = screening.CaseRefused, by, note
	if release {
		c.Status = screening.CaseReleased
	}
	f.cases[id] = c
	f.decided = append(f.decided, id)
	return c, nil
}

func TestScreeningHandlers_TheOperatorSeesTheListsAndDecidesAHeldCase(t *testing.T) {
	f := &fakeScreening{cases: map[string]screening.Case{
		"cc_held":    {ID: "cc_held", Status: screening.CaseHeld, Name: "Mohamad Taher Anwari"},
		"cc_blocked": {ID: "cc_blocked", Status: screening.CaseBlocked, Name: "Banco Nacional de Cuba"},
	}}
	r := chi.NewRouter()
	r.Get("/v1/admin/screening", newScreeningOverviewHandler(f, f).ServeHTTP)
	r.Post("/v1/admin/screening/cases/{caseID}/release", newScreeningDecideHandler(f, true).ServeHTTP)
	r.Post("/v1/admin/screening/cases/{caseID}/refuse", newScreeningDecideHandler(f, false).ServeHTTP)
	do := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(moderatorOperatorHeader, "nicolai")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	code, out := do(http.MethodGet, "/v1/admin/screening?status=held", "")
	lists, _ := out["lists"].([]any)
	cases, _ := out["cases"].([]any)
	if code != http.StatusOK || len(lists) != 2 || len(cases) != 1 || out["fuzzy_threshold_bps"] != float64(9000) {
		t.Fatalf("overview: %d %v", code, out)
	}
	if code, _ := do(http.MethodGet, "/v1/admin/screening?status=maybe", ""); code != http.StatusBadRequest {
		t.Fatalf("an unknown status: %d", code)
	}

	code, out = do(http.MethodPost, "/v1/admin/screening/cases/cc_held/release", `{"note":"a different person"}`)
	c, _ := out["case"].(map[string]any)
	if code != http.StatusOK || c["status"] != screening.CaseReleased || c["decided_by"] != "nicolai" || c["decision_note"] != "a different person" {
		t.Fatalf("release: %d %v", code, out)
	}
	if code, _ := do(http.MethodPost, "/v1/admin/screening/cases/cc_blocked/release", ""); code != http.StatusConflict {
		t.Fatalf("releasing a blocked case: %d", code)
	}
	if code, _ := do(http.MethodPost, "/v1/admin/screening/cases/cc_none/refuse", ""); code != http.StatusNotFound {
		t.Fatalf("refusing no case: %d", code)
	}
	if len(f.decided) != 1 {
		t.Fatalf("decided %v", f.decided)
	}
}
