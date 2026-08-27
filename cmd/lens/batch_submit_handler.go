package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/talyvor/lens/internal/batch"
)

// batch_submit_handler.go — POST /v1/batch/submit, and the workspace it submits
// work under.
//
// ⚠ READ THIS BEFORE READING THE FIX: THE LANE IS CLOSED, IN EVERY CONFIGURATION.
// main.go builds the gate as `newBatchReg(cfg.BatchEnabled, false)` — a literal
// false for settleWired — and newBatchReg is a CONJUNCTION, so `on` cannot be true
// whatever LENS_BATCH_ENABLED says. Executed, not read: newBatchReg(true,false).on
// == false and newBatchReg(false,false).on == false. The /v1/batch/* routes are
// therefore never registered and chi serves its native 404. That closure is
// deliberate and already well pinned (batch_lane_test.go: DefaultsOff,
// FlagAloneCannotOpenAnUnbilledLane, GateActuallyWithholdsTheRoute).
//
// So what follows is a LATENT defect behind a closed door, not a live breach, and
// nothing here should be read with the urgency of a reachable one.
//
// ⚠ WHAT WAS LATENT. The handler read its workspace from a REQUEST HEADER —
// `wsID := req.Header.Get("X-Talyvor-Workspace")`, falling back to "default" when
// absent — and passed it to IsEligible and Submit. No credential was consulted, so
// on the day somebody wires a settle hook and opens the lane, any authenticated
// tenant could submit work billed and attributed to any workspace it cared to
// name, and an unheadered request would land everyone in a shared "default"
// bucket. That is the same shape #146 already decided for every other non-{wsID}
// route, and the same one W6.13 and W6.14 closed on /v1/prompts and
// /v1/eval/cases.
//
// The gun is disarmed here rather than left loaded behind the door, because the
// door is one literal away from opening and the person who opens it will be
// thinking about billing, not tenancy.

// batchSubmitter is the slice of *batch.BatchRouter this route needs.
type batchSubmitter interface {
	IsEligible(body []byte, wsID string) batch.BatchEligibility
	Submit(ctx context.Context, wsID, model, prompt string, body []byte) (*batch.BatchJob, error)
}

func newBatchSubmitHandler(br batchSubmitter) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "read body: "+err.Error())
			return
		}
		// Authz (#146, applied here by W6.17): the header is a REQUEST, not an
		// identity. A non-admin is forced to its own workspace; the global admin
		// still honours the header, and an empty value keeps the router's existing
		// "default" behaviour for it alone.
		wsID, _, ok := effectiveWorkspaceID(req, req.Header.Get("X-Talyvor-Workspace"))
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
			return
		}
		if wsID == "" {
			wsID = "default"
		}
		// Make sure IsEligible sees the batch trigger even when the
		// header — not the body — set it.
		body = ensureBatchFlag(body)
		elig := br.IsEligible(body, wsID)
		if !elig.Eligible {
			writeJSONErr(w, http.StatusBadRequest, elig.Reason)
			return
		}
		var parsed struct {
			Model    string `json:"model"`
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &parsed)
		prompt := ""
		for _, m := range parsed.Messages {
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				prompt += s
			}
		}
		job, err := br.Submit(req.Context(), wsID, parsed.Model, prompt, body)
		if err != nil {
			writeJSONErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSONOK(w, http.StatusAccepted, map[string]any{
			"request_id":           job.RequestID,
			"batch_id":             job.ID,
			"status":               string(job.Status),
			"estimated_completion": "within 24 hours",
			"cost_reduction":       "50%",
		})
	}
}
