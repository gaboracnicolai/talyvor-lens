// Package benchprobe implements proof-of-benchmark MEASUREMENT (P1 #10, PR-A): a verifier draws an
// unpredictable eval item from a verifier-PRIVATE, rotating pool, sends ONLY the input to a node,
// scores the returned text against held ground truth (eval.StaticScore), and records a descriptive
// per-node quality score.
//
// MEASUREMENT ONLY. This package feeds NO routing and NO mint — it imports neither the ledger/mining
// nor any mint path (pinned by TestImportGuard_NoLedgerNoMint). The per-node score is recorded and
// read by nothing in PR-A (routing consumption is PR-B). Delivery is behind ProbeDelivery, FAKED in
// PR-A; the live /inference implementation (gateway token, #242) + the receipt-mint suppression land
// together in PR-A.5.
package benchprobe

import "context"

// EvalItem is one verifier-private pool entry. ExpectedOutput is held verifier-side and is NEVER put
// in a payload sent to a node.
type EvalItem struct {
	ID             string
	Input          string
	ExpectedOutput string
	EvalMethod     string // exact | contains | regex | json_schema (eval.StaticScore)
	PassThreshold  float64
}

// Probe is one recorded (node, item) measurement — the never-reuse ledger row + audit.
type Probe struct {
	ID        string
	NodeID    string
	ItemID    string
	RequestID string // populated by live delivery (PR-A.5); empty in PR-A
	Score     float64
}

// ProbeRequest is the NODE-BLIND payload: input ONLY. There is DELIBERATELY no ExpectedOutput field —
// a node can never receive the ground truth (anti-gaming invariant 2). Shape-identical to a normal
// inference call so a node cannot tell a probe from real traffic (invariant 1, node-blind).
type ProbeRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// BuildProbeRequest constructs the node-blind probe payload from an item — input only, ground truth
// dropped. This is the function the node-blind proof asserts against.
func BuildProbeRequest(model string, item EvalItem) ProbeRequest {
	return ProbeRequest{Model: model, Input: item.Input}
}

// ProbeDelivery sends a probe to a node and returns the node's answer text. requestID is the
// gateway-chosen probe request id (already committed to benchmark_probes); the live delivery sets it
// as X-Request-ID so an HONEST node echoes it into the receipt it submits, where the gateway's
// suppression (request_id ∈ benchmark_probes) records-but-skips the mint. The real implementation
// (HTTPDelivery) routes the input through the node's /inference path with a #242 node-auth token.
type ProbeDelivery interface {
	Deliver(ctx context.Context, nodeID, requestID string, req ProbeRequest) (answer string, err error)
}
