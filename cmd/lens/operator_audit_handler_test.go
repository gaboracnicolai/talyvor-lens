package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/operatoraudit"
)

type auditFake struct{ got []operatoraudit.Entry }

func (f *auditFake) Record(_ context.Context, e operatoraudit.Entry) (operatoraudit.Entry, error) {
	if strings.TrimSpace(e.Actor) == "" {
		return operatoraudit.Entry{}, operatoraudit.ErrInvalid
	}
	f.got = append(f.got, e)
	return e, nil
}

func (f *auditFake) List(context.Context, operatoraudit.Filter) ([]operatoraudit.Entry, error) {
	return f.got, nil
}

// B27.28 — the entry is recorded under the operator X-Talyvor-Operator names; a body naming someone
// else is refused, and so is an entry naming nobody.
func TestOperatorAuditRecord_ActorIsTheNamedOperator(t *testing.T) {
	for _, tc := range []struct {
		header, body string
		code         int
		actor        string
	}{
		{"nicolai@talyvor.com", `{"action":"a"}`, http.StatusCreated, "nicolai@talyvor.com"},
		{"nicolai@talyvor.com", `{"actor":"someone@else.com","action":"a"}`, http.StatusBadRequest, ""},
		{"", `{"actor":"cli:nicolai","action":"a"}`, http.StatusCreated, "cli:nicolai"},
		{"", `{"action":"a"}`, http.StatusBadRequest, ""},
	} {
		store := &auditFake{}
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/operator-audit/record", strings.NewReader(tc.body))
		if tc.header != "" {
			req.Header.Set(moderatorOperatorHeader, tc.header)
		}
		w := httptest.NewRecorder()
		newOperatorAuditRecordHandler(store).ServeHTTP(w, req)
		if w.Code != tc.code || (tc.actor != "" && (len(store.got) != 1 || store.got[0].Actor != tc.actor)) {
			t.Errorf("header %q body %s = %d %s, recorded %+v; want %d under %q", tc.header, tc.body, w.Code, w.Body, store.got, tc.code, tc.actor)
		}
	}
}
