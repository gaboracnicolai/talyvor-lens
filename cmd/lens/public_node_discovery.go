package main

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/talyvor/lens/internal/mining"
)

// public_node_discovery.go — the two UNAUTHENTICATED node-discovery routes, and
// the projection that decides what an anonymous caller may learn about somebody
// else's node.
//
// GET /v1/nodes/available and GET /v1/embedding-nodes/available are registered
// inside the no-auth `pub` group in main.go (rate-limited, nothing else). Their
// handlers used to marshal mining.InferenceNode / mining.EmbeddingNode straight
// to the wire — the SAME structs the OWNER's workspace-scoped
// /v1/workspaces/{wsID}/nodes returns — so every anonymous caller received, for
// every verified+active node of every tenant:
//
//	"workspace_id": who owns the node, and
//	"url":          that tenant's own endpoint.
//
// Extracted to named constructors for the reason #151 gives (see
// authz_attribution_handlers.go): a handler built inline inside run()'s
// dependency graph cannot be driven over HTTP by a test, so the shape of what it
// puts on the wire is unprovable. Named here, it is provable.

// availableNodeLister is the slice of *mining.ComputeMiner this route needs.
type availableNodeLister interface {
	ListAvailableNodes(ctx context.Context, model string) ([]mining.InferenceNode, error)
}

// availableEmbeddingNodeLister is the slice of *mining.EmbeddingMiner this route needs.
type availableEmbeddingNodeLister interface {
	ListAvailableNodes(ctx context.Context, model string, minDimensions int) ([]mining.EmbeddingNode, error)
}

// publicInferenceNode is what an anonymous caller may learn about somebody
// else's GPU node: enough to CHOOSE one, nothing that identifies its owner or
// reaches its machine.
//
// ⚠ WHY `url` IS NOT HERE, AND WHY REMOVING IT COSTS A CALLER NOTHING. A public
// caller cannot use a node URL even if it has one: nodes verify an X-Node-Secret
// that only Lens holds (cmd/node/server.go — the secret is minted by Lens at
// registration and returned to the node, never published), and inference is
// routed to nodes BY LENS, server-side (internal/localrouter). So the URL was
// unusable to anyone entitled to it and useful only to someone attacking the
// node directly or mapping a tenant's private network. Measured, not assumed:
// `nodes/available` appears in no SDK (sdk/python, sdk/typescript), in no
// document, in internal/api/openapi.go, or in any of talyvor-suite,
// talyvor-code, talyvor-track, talyvor-docs — nothing anywhere calls it.
//
// ⚠ WHY `workspace_id` IS NOT HERE. It names which tenant owns which machine at
// which price. That is the tenant roster, and the tenant roster is an ADMIN read
// behind requireAdminOrOperatorRead (/v1/admin/workspaces).
//
// ⚠ WHY `active` AND `verified` ARE NOT HERE. Both were always the literal
// `true` on this route and could not be anything else: the query that feeds it
// selects `WHERE verified = TRUE AND active = TRUE`. A field that is structurally
// constant reads as information and carries none.
//
// ⚠ WHY `ed25519_pubkey` IS NOT HERE. It is a public key and leaks no secret,
// but it is a stable per-node fingerprint whose only consumer is PoVI receipt
// verification INSIDE Lens. A discovery caller has no use for it and a scraper
// would use it to re-link nodes across re-registrations.
type publicInferenceNode struct {
	ID            string    `json:"id"`
	Provider      string    `json:"provider"`
	Models        []string  `json:"models"`
	GPUType       string    `json:"gpu_type"`
	MaxConcurrent int       `json:"max_concurrent"`
	PricePerToken float64   `json:"price_per_token"`
	CreatedAt     time.Time `json:"created_at"`
}

// publicEmbeddingNode is the embedding-node mirror of publicInferenceNode; the
// same reasoning applies field for field.
type publicEmbeddingNode struct {
	ID         string    `json:"id"`
	Model      string    `json:"model"`
	Dimensions int       `json:"dimensions"`
	MaxBatch   int       `json:"max_batch"`
	SpeedTPS   int       `json:"speed_tps"`
	CreatedAt  time.Time `json:"created_at"`
}

// publicView projects the owner-shaped rows onto the anonymous-caller view.
// Always returns a non-nil slice so an empty result is `[]` and not `null`.
func publicNodeView(nodes []mining.InferenceNode) []publicInferenceNode {
	out := make([]publicInferenceNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, publicInferenceNode{
			ID: n.ID, Provider: n.Provider, Models: n.Models, GPUType: n.GPUType,
			MaxConcurrent: n.MaxConcurrent, PricePerToken: n.PricePerToken, CreatedAt: n.CreatedAt,
		})
	}
	return out
}

func publicEmbeddingNodeView(nodes []mining.EmbeddingNode) []publicEmbeddingNode {
	out := make([]publicEmbeddingNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, publicEmbeddingNode{
			ID: n.ID, Model: n.Model, Dimensions: n.Dimensions,
			MaxBatch: n.MaxBatch, SpeedTPS: n.SpeedTPS, CreatedAt: n.CreatedAt,
		})
	}
	return out
}

func newPublicAvailableNodesHandler(lister availableNodeLister) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		model := req.URL.Query().Get("model")
		if model == "" {
			writeJSONErr(w, http.StatusBadRequest, "model query param required")
			return
		}
		nodes, err := lister.ListAvailableNodes(req.Context(), model)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, publicNodeView(nodes))
	}
}

func newPublicAvailableEmbeddingNodesHandler(lister availableEmbeddingNodeLister) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		model := req.URL.Query().Get("model")
		if model == "" {
			writeJSONErr(w, http.StatusBadRequest, "model query param required")
			return
		}
		minDim, _ := strconv.Atoi(req.URL.Query().Get("min_dimensions"))
		nodes, err := lister.ListAvailableNodes(req.Context(), model, minDim)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, publicEmbeddingNodeView(nodes))
	}
}
