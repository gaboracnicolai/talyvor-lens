package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/outputverify"
)

type fakeArtifactCommitter struct{ err error }

func (f fakeArtifactCommitter) Commit(_ context.Context, _, _, _ string, _ []outputverify.ManifestEntry) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	return "sha", true, nil
}

type fakeVerdictAuthn struct{ ws string }

func (f fakeVerdictAuthn) Authenticate(_ *http.Request) (*auth.AuthContext, error) {
	return &auth.AuthContext{WorkspaceID: f.ws}, nil
}

// Error mapping — including the NEW 409 for a content-less output (nothing a buildable tree could ever
// match, so the commitment is impossible; the caller should not retry).
func TestArtifactCommitHandler_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"no content binding → 409", outputverify.ErrNoContentBinding, http.StatusConflict},
		{"not found → 404", outputverify.ErrOutputNotFound, http.StatusNotFound},
		{"not owner → 403", outputverify.ErrNotOutputOwner, http.StatusForbidden},
		{"no output path → 400", outputverify.ErrNoOutputPath, http.StatusBadRequest},
		{"ok → 200", nil, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newArtifactCommitHandler(fakeVerdictAuthn{ws: "wsX"}, fakeArtifactCommitter{err: c.err})
			req := httptest.NewRequest(http.MethodPost, "/v1/outputs/oid-1/artifact",
				strings.NewReader(`{"output_path":"gen.go","context_manifest":[]}`))
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("output_id", "oid-1")
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
			w := httptest.NewRecorder()
			h(w, req)
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d (body=%s)", w.Code, c.want, w.Body.String())
			}
		})
	}
}
