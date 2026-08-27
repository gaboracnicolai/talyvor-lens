package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/localrouter"
)

// local_endpoints_list_handler_test.go — the response of GET /v1/local/endpoints
// against the contract this binary publishes for it.
//
// The contract is not a matter of opinion: internal/api/openapi.go is served at
// GET /openapi.json, and its components.schemas.LocalEndpoint declares the
// properties a client may expect. Everything below compares that declaration with
// the keys the route actually puts on the wire, in BOTH directions — an
// undeclared field shipped, and a declared field missing, are different defects
// and each has its own assertion.

type fakeEndpointLister struct{ eps []*localrouter.LocalEndpoint }

func (f *fakeEndpointLister) List() []*localrouter.LocalEndpoint { return f.eps }

// twoTenantsEndpoints — one endpoint owned by ws-A, one by ws-B, plus a static
// config endpoint (WorkspaceID == "", which is how main.go's syncer marks the
// ones it must never remove). None of them belongs to the caller.
func twoTenantsEndpoints() []*localrouter.LocalEndpoint {
	return []*localrouter.LocalEndpoint{
		{
			ID: "ep-a", WorkspaceID: "ws-A", URL: "http://gpu-a.internal.acme.example:11434",
			Provider: "ollama", Models: []string{"llama3"}, Priority: 1,
			MaxConcurrent: 2, Active: true, Healthy: true, AvgLatencyMs: 12, ErrorRate: 0.1,
		},
		{
			ID: "ep-b", WorkspaceID: "ws-B", URL: "http://10.9.9.9:8000",
			Provider: "vllm", Models: []string{"mistral"}, Priority: 2,
			MaxConcurrent: 4, Active: true, Healthy: true, AvgLatencyMs: 20, ErrorRate: 0,
		},
		{
			ID: "ep-static", URL: "http://localhost:11434",
			Provider: "ollama", Models: []string{"llama3"}, Active: true, Healthy: true,
		},
	}
}

// declaredLocalEndpointProperties reads the property names out of the served
// OpenAPI document rather than out of a list typed here — a hand-typed copy of a
// schema is a second source of truth and drifts from the first one silently.
func declaredLocalEndpointProperties(t *testing.T) []string {
	t.Helper()
	spec := api.OpenAPISpec()
	comps, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatal("the served OpenAPI document has no components — the parse below would compare " +
			"against an empty set and pass over anything")
	}
	schemas, ok := comps["schemas"].(map[string]any)
	if !ok {
		t.Fatal("the served OpenAPI document has no components.schemas")
	}
	le, ok := schemas["LocalEndpoint"].(map[string]any)
	if !ok {
		t.Fatal("the served OpenAPI document declares no LocalEndpoint schema, so this route's " +
			"response has no contract to be compared with")
	}
	props, ok := le["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		t.Fatal("LocalEndpoint declares no properties — an empty contract accepts every response")
	}
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func listEndpoints(t *testing.T, h http.HandlerFunc) []map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/local/endpoints", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	return out
}

func TestLocalEndpointsList_ShipsNoFieldTheContractDoesNotDeclare(t *testing.T) {
	declared := declaredLocalEndpointProperties(t)
	declaredSet := map[string]bool{}
	for _, p := range declared {
		declaredSet[p] = true
	}
	// The finding, pinned by name so nobody has to rediscover which field it was.
	if declaredSet["workspace_id"] {
		t.Fatal("the published LocalEndpoint schema now DECLARES workspace_id. That is a decision " +
			"about publishing which tenant owns which node — take it deliberately and rewrite " +
			"this test with the reason, do not just let it pass")
	}

	rows := listEndpoints(t, newLocalEndpointsListHandler(&fakeEndpointLister{eps: twoTenantsEndpoints()}))

	// NON-VACUITY: rows really came back, and they really are other tenants' rows.
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 — an empty response satisfies every 'must not contain' "+
			"assertion below while proving nothing", len(rows))
	}
	if rows[0]["id"] != "ep-a" {
		t.Fatalf("the response is not the fixture (first id = %v)", rows[0]["id"])
	}

	for i, row := range rows {
		for k := range row {
			if !declaredSet[k] {
				t.Errorf("row %d ships %q, which components.schemas.LocalEndpoint does not declare.\n"+
					"    declared: %s\n"+
					"    This route is in the `authed` group with no further gate, so an undeclared "+
					"field goes to every authenticated caller — and POST and DELETE on this same "+
					"path are both requireAdmin.", i, k, strings.Join(declared, ", "))
			}
		}
	}
}

// The other direction. A declared property the response never carries is a
// promise the API does not keep, and it is the failure that looks like nothing.
func TestLocalEndpointsList_KeepsEveryFieldTheContractDeclares(t *testing.T) {
	declared := declaredLocalEndpointProperties(t)
	rows := listEndpoints(t, newLocalEndpointsListHandler(&fakeEndpointLister{eps: twoTenantsEndpoints()}))
	if len(rows) == 0 {
		t.Fatal("no rows — nothing to compare against the contract")
	}
	for _, p := range declared {
		if _, ok := rows[0][p]; !ok {
			t.Errorf("the contract declares %q and the response does not carry it. Narrowing this "+
				"route was supposed to drop only what was never promised.", p)
		}
	}
}

// The specific consequence, asserted on raw bytes rather than key names so a
// rename (workspace_id → owner) cannot launder it.
func TestLocalEndpointsList_DoesNotNameTheOwningTenant(t *testing.T) {
	eps := twoTenantsEndpoints()
	// ARMED: the owner really is in the input, so "it is not in the output" is a
	// property of the handler and not of the fixture.
	raw, err := json.Marshal(eps)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	for _, needle := range []string{"ws-A", "ws-B"} {
		if !strings.Contains(string(raw), needle) {
			t.Fatalf("the fixture does not contain %q, so the assertion below cannot fail", needle)
		}
	}

	rec := httptest.NewRecorder()
	newLocalEndpointsListHandler(&fakeEndpointLister{eps: eps})(rec,
		httptest.NewRequest(http.MethodGet, "/v1/local/endpoints", nil))
	body := rec.Body.String()

	for _, must := range []string{"ep-a", "ep-b", "ollama", "vllm"} {
		if !strings.Contains(body, must) {
			t.Fatalf("the response is missing %q — it would satisfy the assertions below by "+
				"answering nothing. body=%s", must, body)
		}
	}
	for _, leaked := range []string{"ws-A", "ws-B"} {
		if strings.Contains(body, leaked) {
			t.Errorf("GET /v1/local/endpoints named the owning tenant (%q) to a caller that is "+
				"merely authenticated. body=%s", leaked, body)
		}
	}
}

// ⚠ THE HALF THIS MERGE DELIBERATELY DOES NOT CHANGE, PINNED SO IT CANNOT BE
// MISTAKEN FOR AN OVERSIGHT. `url` is another tenant's endpoint and it is the
// more sensitive field — and the published schema DECLARES it, so removing it is
// a change to a documented contract and a decision, not a repair. This test
// records that it is still there, on purpose. If somebody decides to remove it,
// this is the test that should be deleted with the decision written next to it.
func TestLocalEndpointsList_StillCarriesURL_ByDecisionNotOversight(t *testing.T) {
	rows := listEndpoints(t, newLocalEndpointsListHandler(&fakeEndpointLister{eps: twoTenantsEndpoints()}))
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	if _, ok := rows[0]["url"]; !ok {
		t.Error("url is gone from the response. The published schema declares it, so if this was " +
			"deliberate the schema must change with it and W6.15's note must be answered")
	}
}

// ── the wiring ──

func TestLocalEndpointsListRouteGoesThroughTheProjection(t *testing.T) {
	src := readMainGo(t)
	const want = `authed.Get("/v1/local/endpoints", newLocalEndpointsListHandler(localRouterMulti))`
	if !strings.Contains(src, want) {
		t.Errorf("main.go does not register GET /v1/local/endpoints through " +
			"newLocalEndpointsListHandler; the projection is unreached and every other test in " +
			"this file drives a handler the binary does not serve")
	}
	// ⚠ NOT "main.go never calls List()". It legitimately does, once, inside the
	// `local_models` health check, which COUNTS endpoints and serves none — and the
	// first draft of this assertion flagged exactly that. What must not exist is a
	// call whose result is written to a RESPONSE.
	for _, shape := range []string{
		"writeJSONOK(w, http.StatusOK, localRouterMulti.List())",
		"writeJSONOK(w, http.StatusOK, l.List())",
	} {
		if strings.Contains(src, shape) {
			t.Errorf("main.go writes the raw endpoint list to a response (%s) — that is the "+
				"struct shape, carrying workspace_id, last_check_at and active_count, none of "+
				"which components.schemas.LocalEndpoint declares", shape)
		}
	}
}

// ⚠ THE GAP THE CONTROL CAMPAIGN FOUND IN THE TWO CONTRACT TESTS ABOVE, CLOSED.
// Both of them compare KEY SETS, and a key set cannot tell a projected value from
// a zero one: drop `Active: e.Active` from the view and the response still carries
// `"active": false`, so the contract check passes over a field that stopped
// meaning anything. Control H3's first draft did exactly that and was recorded as
// MISSED rather than talked out of. This asserts the values.
func TestLocalEndpointsList_CarriesTheSourceValuesNotJustTheKeys(t *testing.T) {
	src := twoTenantsEndpoints()
	rows := listEndpoints(t, newLocalEndpointsListHandler(&fakeEndpointLister{eps: src}))
	if len(rows) != len(src) {
		t.Fatalf("got %d rows, want %d", len(rows), len(src))
	}
	want := src[0]
	got := rows[0]
	for _, c := range []struct {
		key  string
		want any
	}{
		{"id", want.ID},
		{"url", want.URL},
		{"provider", want.Provider},
		{"priority", float64(want.Priority)},
		{"max_concurrent", float64(want.MaxConcurrent)},
		{"active", want.Active},
		{"healthy", want.Healthy},
		{"avg_latency_ms", float64(want.AvgLatencyMs)},
		{"error_rate", want.ErrorRate},
	} {
		if got[c.key] != c.want {
			t.Errorf("%s = %v (%T), want %v (%T) — the key is present and the value is not the "+
				"source's, which the key-set contract checks cannot see",
				c.key, got[c.key], got[c.key], c.want, c.want)
		}
	}
	models, _ := got["models"].([]any)
	if len(models) != len(want.Models) {
		t.Errorf("models = %v, want %v", got["models"], want.Models)
	}
}
