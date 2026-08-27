package apicontract

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/ratelimit"
)

// error_contract_test.go — the error bodies this API actually puts on the wire, against the error
// schema it publishes.
//
// ⚠ THESE ARE REAL RESPONSES, NOT PARSED SOURCE. Both middlewares are constructed and driven, and
// the assertions read the bytes they wrote. W6.29 compared the published PATH list with the routes
// and found the paths sound; it did not look at bodies. This is the one body every route shares.
//
// ── WHAT IS PUBLISHED, MEASURED FROM THE SERVED DOCUMENT ───────────────────────────────────────
//
// Of 15 published operations, exactly ONE declares error responses: POST /v1/proxy/openai/{path},
// which declares 401 and 429, both `$ref`-ing components.schemas.APIError. APIError requires `code`
// and `message`.
//
// ── WHAT IS SENT ───────────────────────────────────────────────────────────────────────────────
//
//	401  {"error":"unauthorized"}                                    internal/auth/manager.go
//	429  {"error":…,"limit_type":…,"retry_after_seconds":…}          internal/ratelimit/middleware.go
//
// Neither carries `code` or `message`. ⚠ THE ONE OPERATION THAT DOCUMENTS ITS ERRORS DOCUMENTS THEM
// WRONGLY, and it documents the 401 — the response a client is most likely to code against.
//
// ⚠ NOTHING IS CHANGED HERE AND THAT IS THE POINT OF THE ITEM (W6.30). Both repairs are decisions:
// making the wire match the schema changes the body of every error response in the product, which
// breaks any client already parsing `{"error": …}`; making the schema match the wire discards a
// designed contract — nine ErrCode constants (UNAUTHORIZED, FORBIDDEN, RATE_LIMITED,
// SPEND_CAP_EXCEEDED, …) that exist in internal/api and are emitted NOWHERE, appearing only as an
// `example` inside this same document. Picking either is an API decision, not a repair.
//
// These tests assert TODAY'S state, deliberately, and say what a decision would change. They fail
// when EITHER side moves — including when somebody fixes it, which is the signal to close W6.30
// and delete this file with the reason, not to edit the assertions.

// requiredAPIErrorFields reads the required list out of the SERVED document rather than a copy
// typed here — a hand-written duplicate of a schema is a second source of truth.
func requiredAPIErrorFields(t *testing.T) []string {
	t.Helper()
	comps, _ := api.OpenAPISpec()["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)
	ae, ok := schemas["APIError"].(map[string]any)
	if !ok {
		t.Fatal("the served document declares no APIError schema — W6.30's whole comparison is " +
			"against it, so its absence is a change that needs saying out loud")
	}
	raw, ok := ae["required"].([]string)
	if !ok || len(raw) == 0 {
		t.Fatal("APIError declares no required fields — every body would satisfy it and this file " +
			"would prove nothing")
	}
	out := append([]string(nil), raw...)
	sort.Strings(out)
	return out
}

// operationsDeclaringErrors returns "METHOD path -> status" for every published error response.
func operationsDeclaringErrors(t *testing.T) []string {
	t.Helper()
	paths, _ := api.OpenAPISpec()["paths"].(map[string]any)
	var out []string
	for p, v := range paths {
		ops, _ := v.(map[string]any)
		for m, o := range ops {
			od, _ := o.(map[string]any)
			resp, _ := od["responses"].(map[string]any)
			for code := range resp {
				if strings.HasPrefix(code, "4") || strings.HasPrefix(code, "5") {
					out = append(out, strings.ToUpper(m)+" "+p+" -> "+code)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func bodyKeys(t *testing.T, b []byte) []string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("response body is not a JSON object: %v (body=%q)", err, b)
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── the published side ──────────────────────────────────────────────────────────────────────────

// ⚠ 2 OF 15. Recorded because "the document says nothing about errors" and "the document says
// something wrong about errors" are different facts, and only the second is a broken promise.
func TestExactlyTheProxyOpenAIOperationDocumentsItsErrors(t *testing.T) {
	got := operationsDeclaringErrors(t)
	want := []string{
		"POST /v1/proxy/openai/{path} -> 401",
		"POST /v1/proxy/openai/{path} -> 429",
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("the set of published error responses changed.\n  now:   %v\n  W6.30: %v\n\n"+
			"    MORE means a new promise about an error body — check it against what is actually "+
			"sent, because the two that exist today are not met.\n"+
			"    FEWER means somebody removed a promise; say whether the body changed with it.",
			got, want)
	}
	// ⚠ AND THE IDENTICALLY-SHAPED SIBLING DECLARES NOTHING. /v1/proxy/anthropic/{path} is the same
	// route through the same middleware and documents no error at all — so the document is not
	// merely incomplete, it is inconsistent with itself.
	for _, op := range got {
		if strings.Contains(op, "anthropic") {
			t.Errorf("%s now documents an error response. Good — but it must be checked against the "+
				"wire, which is what the rest of this file does for the openai twin.", op)
		}
	}
}

// ── the wire side: real middleware, real bytes ──────────────────────────────────────────────────

// ⚠ THE 401 IS DRIVEN THROUGH auth.Manager.Middleware WITH NO CREDENTIAL — the exact rejection a
// client hits on POST /v1/proxy/openai/{path} without a key.
func unauthorisedResponse(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	m := auth.NewManager("some-global-key", nil, nil, nil)
	h := m.Middleware(nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("the request was authorised — this test needs the REJECTION path, so it is " +
			"measuring nothing")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/chat/completions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — the fixture is not producing the documented response", rec.Code)
	}
	return rec
}

// ⚠ THE 429 IS DRIVEN THROUGH THE REAL LIMITER against miniredis, with a rule of one request per
// second, so the second call is a genuine rejection rather than a hand-built recorder.
func rateLimitedResponse(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	l := ratelimit.New(rdb, []ratelimit.RateRule{{RequestsPerSecond: 1}})
	h := ratelimit.RateLimitMiddleware(l)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var rec *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		rec = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/chat/completions", nil)
		req.Header.Set("X-Talyvor-Workspace", "ws-limit")
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			return rec
		}
	}
	t.Fatalf("six requests against a 1/s limit never produced a 429 (last status %d) — the "+
		"fixture is not exercising the documented response", rec.Code)
	return nil
}

// ⚠ THE FINDING. Both documented error responses are emitted without either field the schema makes
// required, and with a field it does not declare.
func TestTheDocumentedErrorResponsesDoNotMatchTheDocument(t *testing.T) {
	required := requiredAPIErrorFields(t)

	for _, c := range []struct {
		name string
		rec  *httptest.ResponseRecorder
	}{
		{"401 (auth.Manager.Middleware)", unauthorisedResponse(t)},
		{"429 (ratelimit.RateLimitMiddleware)", rateLimitedResponse(t)},
	} {
		keys := bodyKeys(t, c.rec.Body.Bytes())
		have := map[string]bool{}
		for _, k := range keys {
			have[k] = true
		}
		var missing []string
		for _, r := range required {
			if !have[r] {
				missing = append(missing, r)
			}
		}
		if len(missing) != len(required) {
			t.Errorf("%s now carries %d of the %d fields APIError requires (%v). W6.30 measured it "+
				"carrying NONE of them.\n"+
				"    If the wire is being brought to the schema, finish it — a body with `code` but "+
				"no `message` satisfies neither the document nor a client.\n"+
				"    If it is finished, this file has done its job: delete it and close W6.30.",
				c.name, len(required)-len(missing), len(required), keys)
		}
		if !have["error"] {
			t.Errorf("%s no longer carries the undeclared `error` key (keys: %v). That is the field "+
				"every caller of this API parses today; changing it is a breaking change and needs "+
				"saying, not just doing.", c.name, keys)
		}
		t.Logf("MEASURED %s: body keys %v; APIError requires %v — no overlap.", c.name, keys, required)
	}
}

// ⚠ THE DESIGNED CONTRACT THAT IS WIRED TO NOTHING. Nine ErrCode constants exist in internal/api.
// Their only appearance outside their own declaration is as an `example` on APIError.code — a field
// of a schema no response satisfies. This is the same shape as W6.25's unwired seams and W6.27's
// unread config: a capability fully built and connected at neither end.
func TestTheErrorCodeConstantsAreEmittedNowhere(t *testing.T) {
	codes := []string{
		api.ErrCodeUnauthorized, api.ErrCodeForbidden, api.ErrCodeNotFound,
		api.ErrCodeRateLimited, api.ErrCodeSpendCapExceeded, api.ErrCodeInvalidRequest,
		api.ErrCodeInternalError, api.ErrCodeModelNotAllowed, api.ErrCodeProviderUnavailable,
	}
	bodies := []string{
		unauthorisedResponse(t).Body.String(),
		rateLimitedResponse(t).Body.String(),
	}
	for _, code := range codes {
		for i, b := range bodies {
			if strings.Contains(b, code) {
				t.Errorf("error code %q now appears in response %d (%s). W6.30 measured every one of "+
					"the nine as emitted nowhere; if the API has started using them, the APIError "+
					"schema and every client's parser need to move together.", code, i, b)
			}
		}
	}
	t.Logf("MEASURED: %d declared error codes, %d emitted on the two documented error paths.",
		len(codes), 0)
}
