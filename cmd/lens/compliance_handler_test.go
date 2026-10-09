package main

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/compliance"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/stepup"
)

// B30.8 — a case action needs the admin key, the operator's name and a current step-up code, used once; the freeze
// route freezes as that operator and answers the case file. The case file itself is faked: internal/compliance's
// real-PG test covers it.

type fakeCaseFile struct{ frozenBy, reason string }

func (f *fakeCaseFile) Cases(context.Context, compliance.Filter, int) ([]screening.Case, error) {
	return nil, nil
}
func (f *fakeCaseFile) Freeze(context.Context, string) (*economy.Freeze, error) { return nil, nil }
func (f *fakeCaseFile) File(_ context.Context, id string) (compliance.File, error) {
	file := compliance.File{Case: screening.Case{ID: id}}
	if f.frozenBy != "" {
		file.Freeze = &economy.Freeze{CaseID: id, FrozenBy: f.frozenBy, Reason: f.reason}
	}
	return file, nil
}
func (f *fakeCaseFile) AddNote(context.Context, string, string, string) error { return nil }
func (f *fakeCaseFile) FreezeWorkspace(_ context.Context, _ string, by, reason string) (economy.Freeze, error) {
	f.frozenBy, f.reason = by, reason
	return economy.Freeze{}, nil
}
func (f *fakeCaseFile) UnfreezeWorkspace(context.Context, string, string, string) error { return nil }
func (f *fakeCaseFile) Close(context.Context, string, string, string) (screening.Case, error) {
	return screening.Case{}, nil
}
func (f *fakeCaseFile) Export(context.Context, string, string) (string, error) { return "", nil }

func TestComplianceFreeze_BehindStepUp(t *testing.T) {
	key := []byte("0123456789abcdef0123")
	v, err := stepup.New(base32.StdEncoding.EncodeToString(key), nil)
	if err != nil {
		t.Fatal(err)
	}
	cf := &fakeCaseFile{}
	r := chi.NewRouter()
	r.Post("/v1/admin/compliance/cases/{caseID}/freeze", requireStepUp(headerAdmin{}, v, newComplianceActionHandler(cf, compliance.ActionFreeze)))
	unset, _ := stepup.New("", nil)
	r.Post("/unset/{caseID}/freeze", requireStepUp(headerAdmin{}, unset, newComplianceActionHandler(cf, compliance.ActionFreeze)))
	do := func(path, admin, operator, code string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason":"pending the owner's explanation"}`))
		if admin != "" {
			req.Header.Set("Authorization", "Bearer "+admin)
		}
		req.Header.Set(moderatorOperatorHeader, operator)
		req.Header.Set(stepUpHeader, code)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	const path = "/v1/admin/compliance/cases/cc_1/freeze"
	code := stepup.CodeAt(key, time.Now())

	for _, c := range []struct {
		name, admin, operator, code string
		want                        int
	}{
		{"no admin key", "", "nicolai", code, http.StatusUnauthorized},
		{"a workspace key", "ws-key", "nicolai", code, http.StatusUnauthorized},
		{"no operator named", "admin-key", "", code, http.StatusBadRequest},
		{"no step-up code", "admin-key", "nicolai", "", http.StatusForbidden},
		{"a wrong code", "admin-key", "nicolai", "000000", http.StatusForbidden},
	} {
		if got, body := do(path, c.admin, c.operator, c.code); got != c.want {
			t.Errorf("%s: %d %s; want %d", c.name, got, body, c.want)
		}
	}
	if cf.frozenBy != "" {
		t.Fatalf("a refused request froze the workspace, as %q", cf.frozenBy)
	}

	got, body := do(path, "admin-key", "nicolai", code)
	if got != http.StatusOK {
		t.Fatalf("with a current code: %d %s", got, body)
	}
	var file compliance.File
	if err := json.Unmarshal([]byte(body), &file); err != nil || file.Freeze == nil || file.Freeze.FrozenBy != "nicolai" ||
		cf.reason != "pending the owner's explanation" {
		t.Fatalf("the case file answered = %s (%v); frozen by %q for %q", body, err, cf.frozenBy, cf.reason)
	}
	if got, _ := do(path, "admin-key", "nicolai", code); got != http.StatusForbidden {
		t.Fatalf("the same code again: %d; want 403", got)
	}
	if got, _ := do("/unset/cc_1/freeze", "admin-key", "nicolai", code); got != http.StatusServiceUnavailable {
		t.Fatalf("no step-up secret set: %d; want 503", got)
	}
}
