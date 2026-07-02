package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// attestation.go — the node-side producer of a hardware attestation report (Proof-of-Confidential-Compute,
// step a). INERT by default: no Attestor is wired unless the operator sets NODE_ATTESTATION_ENABLED +
// NODE_ATTESTATION_CMD, and a nil Attestor makes /attestation return 501. The daemon does NOT verify the
// EAT — it relays a fresh gateway nonce to the producer and passes the producer's signed EAT back (wrapped
// in the node's ed25519 identity signature); the NVIDIA signature is verified gateway-side (step b).

// Attestor produces a hardware attestation token bound to a gateway-supplied nonce. On real CC hardware the
// token is an NVIDIA NRAS EAT whose eat_nonce == nonce.
type Attestor interface {
	Report(ctx context.Context, nonce int64) (eatJWT string, err error)
}

// execAttestor shells out to NVIDIA's attestation tooling (nvtrust / local GPU verifier → NRAS), passing
// the nonce, and returns the signed EAT from stdout. The command line is operator-configured
// (NODE_ATTESTATION_CMD) and swappable — there is no Go-native NVIDIA attestation library, so the daemon
// orchestrates the external producer. Example: "nvtrust-attest --format eat" → we append "--nonce <n>".
type execAttestor struct {
	name string
	args []string
}

// newExecAttestor parses an operator command line ("tool --flag ...") into an exec target. Returns nil for
// an empty command (⇒ no attestor wired ⇒ endpoint inert).
func newExecAttestor(cmdline string) *execAttestor {
	fields := strings.Fields(cmdline)
	if len(fields) == 0 {
		return nil
	}
	return &execAttestor{name: fields[0], args: fields[1:]}
}

// Report runs the configured tool with the nonce appended as "--nonce <n>" and returns its trimmed stdout
// (the EAT). A non-zero exit or empty output is an error — the handler surfaces it as 502, never a mint.
func (e *execAttestor) Report(ctx context.Context, nonce int64) (string, error) {
	args := append(append([]string{}, e.args...), "--nonce", strconv.FormatInt(nonce, 10))
	// e.name/e.args come from NODE_ATTESTATION_CMD — OPERATOR config, same trust boundary as the daemon
	// binary itself (a node operator who can set this could already run any code on their own box). The only
	// network-derived value, `nonce`, is a strconv.FormatInt integer passed as a DISCRETE argv element (no
	// shell, no interpolation) — so no untrusted input can alter the command. Intentional by design (the
	// NVIDIA attestation tool has no Go library; the daemon must shell out to a swappable operator tool).
	//nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	out, err := exec.CommandContext(ctx, e.name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("node: attestation command %q failed: %w", e.name, err)
	}
	eat := strings.TrimSpace(string(out))
	if eat == "" {
		return "", fmt.Errorf("node: attestation command %q produced no report", e.name)
	}
	return eat, nil
}
