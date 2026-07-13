package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/attest"
)

type fakeAttestor struct {
	gotOutputID string
	gotTree     []byte
	res         attest.Result
}

func (f *fakeAttestor) Attest(_ context.Context, outputID string, tree []byte) (attest.Result, error) {
	f.gotOutputID = outputID
	f.gotTree = tree
	return f.res, nil
}

func TestAttest_Handler(t *testing.T) {
	fa := &fakeAttestor{res: attest.Result{Outcome: attest.OutcomeAttested, Verdict: "compile_failed", Recorded: true}}
	r := chi.NewRouter()
	r.Post("/v1/admin/attest/{output_id}", newAttestHandler(fa))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/attest/oid-xyz", strings.NewReader("TREE-BYTES")))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	if fa.gotOutputID != "oid-xyz" || string(fa.gotTree) != "TREE-BYTES" {
		t.Errorf("handler must pass the path output_id + body to the attestor; got id=%q tree=%q", fa.gotOutputID, string(fa.gotTree))
	}
}
