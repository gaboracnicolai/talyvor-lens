package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/tare"
)

type halveProse struct{}

func (halveProse) Reduce(_ context.Context, content []byte, _ tare.Kind) ([]byte, int, int, error) {
	w := strings.Fields(string(content))
	out := []byte(strings.Join(w[:len(w)/2], " "))
	return out, tare.EstimateTokens(content), tare.EstimateTokens(out), nil
}

// B27.35: the Try-it preview shows what a workspace's requests get — prose is reduced by phase 2a only
// for a workspace that opted in, and the answer says whether the model was in the run.
func TestTarePreview_ProseUsesTheModelOnlyWhenTheWorkspaceOptedIn(t *testing.T) {
	prose := strings.Repeat("the deploy waits for the platform team to finish their change first ", 4)
	for _, optedIn := range []bool{true, false} {
		r := chi.NewRouter()
		r.Post("/v1/workspaces/{wsID}/tare/preview", newTarePreviewHandler(func(wsID string) *tare.Reducer {
			if !optedIn || wsID != "ws-1" {
				return nil
			}
			return &tare.Reducer{Kind: tare.KindProse, New: func(func(tare.Refusal)) tare.Reduction { return halveProse{} }}
		}))
		body, _ := json.Marshal(map[string]string{"content": prose})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws-1/tare/preview", strings.NewReader(string(body))))
		var out struct {
			Kind      string `json:"kind"`
			Refused   bool   `json:"refused"`
			TareModel bool   `json:"tare_model"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
			t.Fatalf("opted in=%v: status %d, body %s", optedIn, w.Code, w.Body.String())
		}
		if optedIn && (out.Kind != "prose" || out.Refused || !out.TareModel) {
			t.Errorf("opted in: %+v, want kind prose, not refused, tare_model true", out)
		}
		if !optedIn && (!out.Refused || out.TareModel) {
			t.Errorf("not opted in: %+v, want refused and tare_model false", out)
		}
	}
}
