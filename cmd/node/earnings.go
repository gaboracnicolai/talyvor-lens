package main

// earnings.go — formats the LENS earnings dashboard the
// `talyvor-node earnings` command renders. Keeps the formatting
// off the main wiring code so it can be unit-tested.

import (
	"fmt"
	"strings"
)

// LENSToUSD converts a LENS amount to its display USD value.
// Hard-coded at the 1 LENS = $0.10 peg.
const LENSToUSD = 0.10

// FormatEarnings produces the multi-line dashboard string. We
// avoid color/formatting libraries — plain ASCII keeps the
// binary small and works in any terminal.
//
// The maps come straight from the Lens API responses; this
// function picks out the fields it knows about and ignores any
// extras so a newer Lens server with additional fields doesn't
// break older nodes.
func FormatEarnings(state NodeState, earnings, balance map[string]any) string {
	var sb strings.Builder

	gpuLabel := gpuDisplayName(state.GPUType)

	sb.WriteString("Talyvor Node Earnings\n")
	sb.WriteString("─────────────────────\n")
	fmt.Fprintf(&sb, "Node ID:         %s\n", state.NodeID)
	fmt.Fprintf(&sb, "GPU Type:        %s\n", gpuLabel)
	fmt.Fprintf(&sb, "Status:          Online ✅\n")
	sb.WriteString("\n")

	// We don't have today/month breakdowns from the API directly
	// — we report the headline lifetime number from earnings +
	// the balance row.
	earnedLifetime := floatField(earnings, "earned_total")
	tokensServed := int64Field(earnings, "tokens_served_total")
	nodesActive := intField(earnings, "nodes_active")

	balanceCurrent := floatField(balance, "balance")
	lifetimeEarned := floatField(balance, "lifetime_earned")

	sb.WriteString("Compute mining (lifetime):\n")
	fmt.Fprintf(&sb, "  Active nodes:    %d\n", nodesActive)
	fmt.Fprintf(&sb, "  Tokens served:   %s\n", formatNumber(tokensServed))
	fmt.Fprintf(&sb, "  Earnings:        %s\n", formatLENS(earnedLifetime))
	sb.WriteString("\n")

	sb.WriteString("Wallet:\n")
	fmt.Fprintf(&sb, "  Current balance: %s\n", formatLENS(balanceCurrent))
	fmt.Fprintf(&sb, "  Lifetime earned: %s\n", formatLENS(lifetimeEarned))
	sb.WriteString("\n")

	return sb.String()
}

// FormatStatus renders the short `talyvor-node status` output.
func FormatStatus(state NodeState, healthy bool) string {
	var sb strings.Builder
	sb.WriteString("Talyvor Node Status\n")
	sb.WriteString("───────────────────\n")
	fmt.Fprintf(&sb, "Node ID:        %s\n", state.NodeID)
	fmt.Fprintf(&sb, "Workspace:      %s\n", state.WorkspaceID)
	fmt.Fprintf(&sb, "Lens URL:       %s\n", state.LensURL)
	fmt.Fprintf(&sb, "Node URL:       %s\n", state.NodeURL)
	fmt.Fprintf(&sb, "Provider:       %s\n", state.Provider)
	fmt.Fprintf(&sb, "GPU type:       %s\n", gpuDisplayName(state.GPUType))
	fmt.Fprintf(&sb, "Models:         %s\n", strings.Join(state.Models, ", "))
	if healthy {
		sb.WriteString("Health:         ✅ healthy\n")
	} else {
		sb.WriteString("Health:         ❌ unhealthy\n")
	}
	fmt.Fprintf(&sb, "Registered:     %s\n", state.RegisteredAt.Format("2006-01-02 15:04:05"))
	return sb.String()
}

// ─── small helpers ───────────────────────────────

// gpuDisplayName turns the canonical lowercase code into the
// pretty label the dashboard renders.
func gpuDisplayName(gpu string) string {
	switch strings.ToLower(gpu) {
	case "cpu":
		return "CPU"
	case "rtx4090":
		return "RTX 4090"
	case "a100":
		return "A100"
	case "h100":
		return "H100"
	}
	return gpu
}

func floatField(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		case int64:
			return float64(n)
		}
	}
	return 0
}

func intField(m map[string]any, key string) int {
	return int(int64Field(m, key))
}

func int64Field(m map[string]any, key string) int64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int:
			return int64(n)
		case int64:
			return n
		}
	}
	return 0
}

// formatLENS renders "X.YY LENS ($Z.ZZ)" with the LENS-to-USD
// peg applied.
func formatLENS(amount float64) string {
	return fmt.Sprintf("%.4f LENS ($%.2f)", amount, amount*LENSToUSD)
}

// formatNumber inserts thousands separators — the earnings
// dashboard reads better with "1,247" than "1247".
func formatNumber(n int64) string {
	if n == 0 {
		return "0"
	}
	s := fmt.Sprintf("%d", n)
	out := make([]byte, 0, len(s)+len(s)/3)
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
