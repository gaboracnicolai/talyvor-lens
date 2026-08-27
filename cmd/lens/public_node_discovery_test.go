package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/mining"
)

// public_node_discovery_test.go — what an ANONYMOUS caller learns from the two
// no-auth discovery routes.
//
// The premise these tests rest on is measured elsewhere and not assumed here:
//   - that ListAvailableNodes returns EVERY tenant's rows, url and workspace_id
//     populated, is proven against real Postgres in
//     internal/mining/available_nodes_crosstenant_realpg_test.go;
//   - that the two routes really do sit in a no-auth group is proven from the
//     router source in TestPublicDiscoveryRoutesAreRegisteredWithoutAuth below.
//
// So these are the wire-shape half: given rows belonging to somebody else, what
// comes back on an unauthenticated request.

type fakeAvailableNodes struct {
	nodes []mining.InferenceNode
	model string // records what the handler asked for
	err   error
}

func (f *fakeAvailableNodes) ListAvailableNodes(_ context.Context, model string) ([]mining.InferenceNode, error) {
	f.model = model
	return f.nodes, f.err
}

type fakeAvailableEmbeddingNodes struct {
	nodes  []mining.EmbeddingNode
	model  string
	minDim int
	err    error
}

func (f *fakeAvailableEmbeddingNodes) ListAvailableNodes(_ context.Context, model string, minDim int) ([]mining.EmbeddingNode, error) {
	f.model, f.minDim = model, minDim
	return f.nodes, f.err
}

// twoTenantsNodes — one node owned by ws-A, one by ws-B. Neither belongs to the
// caller, because the caller is anonymous and owns nothing.
func twoTenantsNodes() []mining.InferenceNode {
	return []mining.InferenceNode{
		{
			ID: "node-a", WorkspaceID: "ws-A", URL: "http://gpu-a.internal.acme.example:8080",
			Provider: "vllm", Models: []string{"llama-3-70b"}, GPUType: "a100",
			MaxConcurrent: 4, PricePerToken: 0.002, Active: true, Verified: true,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Ed25519PubKey: "AAAApubkeyA",
		},
		{
			ID: "node-b", WorkspaceID: "ws-B", URL: "https://10.4.4.9:9443",
			Provider: "ollama", Models: []string{"llama-3-70b"}, GPUType: "4090",
			MaxConcurrent: 1, PricePerToken: 0.001, Active: true, Verified: true,
			CreatedAt: time.Unix(1_700_000_500, 0).UTC(),
		},
	}
}

func twoTenantsEmbeddingNodes() []mining.EmbeddingNode {
	return []mining.EmbeddingNode{
		{
			ID: "emb-a", WorkspaceID: "ws-A", URL: "http://embed-a.internal.acme.example:7000",
			Model: "bge-large", Dimensions: 1024, MaxBatch: 32, SpeedTPS: 900,
			Active: true, Verified: true, CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			NodeSecretHash: "$2a$10$thisisabcrypthashthatmustnevershipanywhere",
		},
		{
			ID: "emb-b", WorkspaceID: "ws-B", URL: "https://192.168.7.7:7443",
			Model: "bge-large", Dimensions: 1024, MaxBatch: 8, SpeedTPS: 400,
			Active: true, Verified: true, CreatedAt: time.Unix(1_700_000_500, 0).UTC(),
		},
	}
}

// armed asserts that every needle the leak test looks for is ACTUALLY PRESENT in
// the rows the store hands the handler. Without this, gutting the fixture (drop
// the URLs, blank the workspace ids) makes every "must not contain" assertion
// below pass while testing nothing — the exact vacuity this queue keeps catching
// in its own controls. The needles are checked against the OWNER-shaped
// marshalling of the same rows, i.e. what the route used to return.
func armed(t *testing.T, rows any, needles []string) {
	t.Helper()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	for _, n := range needles {
		if !strings.Contains(string(raw), n) {
			t.Fatalf("the fixture does not contain %q, so the leak assertion for it cannot "+
				"fail and proves nothing. fixture=%s", n, raw)
		}
	}
}

func getPublic(t *testing.T, h http.HandlerFunc, target string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	// No Authorization header, no API key, no session cookie: these routes are in
	// the `pub` group and this is the request an anonymous internet caller makes.
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec, rec.Body.String()
}

// ── the leak, on the compute route ──

func TestPublicNodeDiscovery_DoesNotNameTheOwnerOrItsEndpoint(t *testing.T) {
	rows := twoTenantsNodes()
	armed(t, rows, []string{"gpu-a.internal.acme.example", "10.4.4.9", "ws-A", "ws-B"})
	fake := &fakeAvailableNodes{nodes: rows}
	rec, body := getPublic(t, newPublicAvailableNodesHandler(fake), "/v1/nodes/available?model=llama-3-70b")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, body)
	}
	// NON-VACUITY FIRST. A handler that 500s, or returns `null`, or drops the rows
	// would satisfy every "must not contain" assertion below while answering
	// nothing. So prove the discovery answer is actually present before proving
	// what is absent from it.
	for _, must := range []string{"node-a", "node-b", "a100", "vllm", "0.002"} {
		if !strings.Contains(body, must) {
			t.Fatalf("the discovery answer is missing %q — this test would pass on an empty "+
				"response, which is not the property under test. body=%s", must, body)
		}
	}

	// THE PROPERTY. Raw-substring, not field-name, assertions: a rename
	// (`url` → `endpoint`, `workspace_id` → `owner`) must not make this pass.
	for _, leaked := range []struct{ what, needle string }{
		{"ws-A's node URL", "gpu-a.internal.acme.example"},
		{"ws-B's node URL", "10.4.4.9"},
		{"ws-A's identity", "ws-A"},
		{"ws-B's identity", "ws-B"},
	} {
		if strings.Contains(body, leaked.needle) {
			t.Errorf("an UNAUTHENTICATED GET /v1/nodes/available returned %s (%q).\n"+
				"    This route is in the no-auth `pub` group, so that is published to the "+
				"anonymous internet for every verified node of every tenant.\n    body=%s",
				leaked.what, leaked.needle, body)
		}
	}
}

func TestPublicEmbeddingNodeDiscovery_DoesNotNameTheOwnerOrItsEndpoint(t *testing.T) {
	rows := twoTenantsEmbeddingNodes()
	// ⚠ the bcrypt hash is deliberately NOT in this list: mining.EmbeddingNode
	// tags it `json:"-"`, so it is absent from the OWNER shape too and `armed`
	// would fail on it. That field's non-leak is asserted below as a property of
	// the struct tag, and TestNodeSecretHashIsNeverMarshalled pins it directly.
	armed(t, rows, []string{"embed-a.internal.acme.example", "192.168.7.7", "ws-A", "ws-B"})
	fake := &fakeAvailableEmbeddingNodes{nodes: rows}
	rec, body := getPublic(t, newPublicAvailableEmbeddingNodesHandler(fake),
		"/v1/embedding-nodes/available?model=bge-large&min_dimensions=768")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, body)
	}
	for _, must := range []string{"emb-a", "emb-b", "1024", "900"} {
		if !strings.Contains(body, must) {
			t.Fatalf("the discovery answer is missing %q — this test would pass on an empty "+
				"response. body=%s", must, body)
		}
	}
	for _, leaked := range []struct{ what, needle string }{
		{"ws-A's node URL", "embed-a.internal.acme.example"},
		{"ws-B's node URL", "192.168.7.7"},
		{"ws-A's identity", "ws-A"},
		{"ws-B's identity", "ws-B"},
		{"the node secret bcrypt hash", "$2a$10$"},
	} {
		if strings.Contains(body, leaked.needle) {
			t.Errorf("an UNAUTHENTICATED GET /v1/embedding-nodes/available returned %s (%q).\n    body=%s",
				leaked.what, leaked.needle, body)
		}
	}
}

// ── the mirror: the route must still answer the question it exists to answer ──

func TestPublicNodeDiscovery_StillAnswersWhichNodeToChoose(t *testing.T) {
	fake := &fakeAvailableNodes{nodes: twoTenantsNodes()}
	_, body := getPublic(t, newPublicAvailableNodesHandler(fake), "/v1/nodes/available?model=llama-3-70b")

	var got []map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if len(got) != 2 {
		t.Fatalf("got %d nodes, want 2 — narrowing the projection must not drop rows", len(got))
	}
	// The fields a caller needs in order to CHOOSE a node, one by one rather than
	// as a set, so dropping one cannot be absorbed by the others.
	for _, k := range []string{"id", "provider", "models", "gpu_type", "max_concurrent", "price_per_token"} {
		if _, ok := got[0][k]; !ok {
			t.Errorf("public discovery row has no %q — the route no longer answers "+
				"\"which node should I ask for?\"", k)
		}
	}
	// And the query really reached the store, so a hardcoded payload cannot pass.
	if fake.model != "llama-3-70b" {
		t.Errorf("store was asked for model %q, want %q", fake.model, "llama-3-70b")
	}
}

func TestPublicEmbeddingNodeDiscovery_StillAnswersWhichNodeToChoose(t *testing.T) {
	fake := &fakeAvailableEmbeddingNodes{nodes: twoTenantsEmbeddingNodes()}
	_, body := getPublic(t, newPublicAvailableEmbeddingNodesHandler(fake),
		"/v1/embedding-nodes/available?model=bge-large&min_dimensions=768")

	var got []map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if len(got) != 2 {
		t.Fatalf("got %d nodes, want 2", len(got))
	}
	for _, k := range []string{"id", "model", "dimensions", "max_batch", "speed_tps"} {
		if _, ok := got[0][k]; !ok {
			t.Errorf("public embedding discovery row has no %q", k)
		}
	}
	if fake.model != "bge-large" || fake.minDim != 768 {
		t.Errorf("store was asked for (%q,%d), want (%q,%d)", fake.model, fake.minDim, "bge-large", 768)
	}
}

// ── the other mirror: the OWNER's view is untouched ──
//
// Narrowing the public projection is only correct if it narrows the PUBLIC one.
// The workspace-scoped route /v1/workspaces/{wsID}/nodes marshals
// mining.InferenceNode itself and is behind AuthMiddleware + the tenant-isolation
// middleware, so the owner must still get the url of its own node — that is how
// an operator finds the machine it registered.

func TestOwnerScopedNodeShapeStillCarriesURLAndWorkspace(t *testing.T) {
	raw, err := json.Marshal(twoTenantsNodes()[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, k := range []string{`"url"`, `"workspace_id"`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("mining.InferenceNode no longer marshals %s — the OWNER's "+
				"/v1/workspaces/{wsID}/nodes needs it; only the PUBLIC projection was "+
				"supposed to narrow. raw=%s", k, raw)
		}
	}
	rawE, err := json.Marshal(twoTenantsEmbeddingNodes()[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, k := range []string{`"url"`, `"workspace_id"`} {
		if !strings.Contains(string(rawE), k) {
			t.Errorf("mining.EmbeddingNode no longer marshals %s. raw=%s", k, rawE)
		}
	}
}

// ── error and edge paths keep their behaviour ──

func TestPublicNodeDiscovery_RequiresModel(t *testing.T) {
	rec, _ := getPublic(t, newPublicAvailableNodesHandler(&fakeAvailableNodes{}), "/v1/nodes/available")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing model: status = %d, want 400", rec.Code)
	}
	rec, _ = getPublic(t, newPublicAvailableEmbeddingNodesHandler(&fakeAvailableEmbeddingNodes{}),
		"/v1/embedding-nodes/available")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing model (embedding): status = %d, want 400", rec.Code)
	}
}

func TestPublicNodeDiscovery_EmptyResultIsAnEmptyList(t *testing.T) {
	// nil from the store must not become a leaky or confusing payload.
	_, body := getPublic(t, newPublicAvailableNodesHandler(&fakeAvailableNodes{nodes: nil}),
		"/v1/nodes/available?model=nobody-serves-this")
	if strings.TrimSpace(body) == "" {
		t.Error("empty store produced an empty body")
	}
}

// The one field on mining.EmbeddingNode that was already protected, pinned so the
// protection is a tripwire rather than a comment. `json:"-"` is why the leak test
// above never saw the bcrypt hash; if the tag is dropped, this reds and the leak
// test reds with it.
func TestNodeSecretHashIsNeverMarshalled(t *testing.T) {
	raw, err := json.Marshal(twoTenantsEmbeddingNodes()[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "$2a$10$") {
		t.Errorf("mining.EmbeddingNode marshalled node_secret_hash: %s", raw)
	}
	if strings.Contains(string(raw), "node_secret") {
		t.Errorf("mining.EmbeddingNode marshalled a node_secret field: %s", raw)
	}
}
