package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/talyvor/lens/internal/eval"
)

// eval_case_create_handler.go — POST /v1/eval/cases, and the workspace it may
// write to.
//
// ⚠ WHAT THIS ROUTE DID BEFORE. It decoded an eval.TestCase from the request body
// — `workspace_id` and all — and handed it to evalPipeline.AddTestCase. No
// credential was consulted, so the workspace a test case was created IN was
// whatever the caller typed. Same shape as POST /v1/prompts (W6.13) and the same
// rule #146 already decided everywhere else on this binary's non-{wsID} routes.
//
// ⚠ AND A PLANTED CASE IS EXECUTED, ON SOMEBODY ELSE'S BUDGET. eval.Pipeline's
// RunSuite(ctx, workspaceID, tags) lists EVERY case in that workspace and runs
// each one, and runTestCaseWith calls callLLM(ctx, tc.Provider, tc.Model,
// tc.Prompt) — provider, model and prompt all taken from the stored case, i.e. all
// chosen by whoever created it. The call goes out on the OPERATOR's provider keys
// (Pipeline holds openAIKey / anthropicKey / googleKey from config), the result
// carries CostUSD into the victim's RunSummary.TotalCostUSD, and the dataset run
// path attributes the spend to the victim's workspace through SpendRecorder. An
// LLM-judge case makes a second call.
//
// So this is a money path: whoever can create a case in a workspace chooses which
// model that workspace's next eval run pays for, and with what content.

// evalCaseCreator is the slice of *eval.Pipeline this route needs.
type evalCaseCreator interface {
	AddTestCase(ctx context.Context, tc eval.TestCase) (*eval.TestCase, error)
}

func newEvalCaseCreateHandler(p evalCaseCreator) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in eval.TestCase
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		// Authz (#146, applied here by W6.14): a non-admin may create only in its
		// OWN workspace; the global admin honours the body, and an empty value
		// keeps AddTestCase's existing "default" behaviour for it.
		eff, _, ok := effectiveWorkspaceID(req, in.WorkspaceID)
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
			return
		}
		in.WorkspaceID = eff
		created, err := p.AddTestCase(req.Context(), in)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, created)
	}
}
