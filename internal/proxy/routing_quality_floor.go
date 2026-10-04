package proxy

import (
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/routing"
)

// gateQualityFloor labels an auto-route recommendation held by the B27.23
// quality floor on the RoutingTierGated metric.
const gateQualityFloor = "quality_floor"

// SetQualityGate wires B27.23's measured quality floor. Production always wires
// it (cmd/lens); a Proxy built without one routes on the router's pick alone.
func (p *Proxy) SetQualityGate(g *routing.QualityGate) { p.qualityGate = g }

// downgradeKeepsQuality reports whether serving `to` in place of `from` may
// stand. A substitution that is not a strictly cheaper model in the same
// provider (router.ShouldOverride) is not a cost downgrade and passes; a cost
// downgrade passes only when the gate measured `to` at or above `from`'s
// quality on this request's cohort. The reason is the gate's, for the log.
func (p *Proxy) downgradeKeepsQuality(feature string, inputTokens int, provider, from, to string) (bool, string) {
	if p.qualityGate == nil || p.router == nil || from == "" || to == "" {
		return true, ""
	}
	if !p.router.ShouldOverride(from, router.RoutingDecision{Model: to}) {
		return true, ""
	}
	v := p.qualityGate.Downgrade(feature, inputTokens, provider, from, to)
	return v.Allowed, v.Reason
}
