package main

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/econflags"
)

// batch_routes.go — the /v1/batch/* lane switch, the ROUTE chokepoint.
//
// Deliberately a SEPARATE gate from econReg rather than a second user of it. econReg is the
// economy master kill-switch and its manifest test means something specific; this lane is not an
// economy feature, it is an inference lane that currently bills nothing, and conflating the two
// would make the economy killswitch test's coverage claim untrue.
//
// Same mechanism, same reason: when off the route is never registered, so chi serves its native
// 404 — indistinguishable from a path that never existed, and no existence oracle for a lane that
// is not ready to be reached.
//
// Adding another /v1/batch route? Register it through batchGate.{get,post} — batch_lane_test.go
// walks main.go and FAILS on a bare authed.Post for any /v1/batch path, because a bare
// registration is exactly how this lane shipped unbilled.
type batchReg struct{ on bool }

// newBatchReg decides whether the lane may be reachable AT ALL.
//
// ⚠ THE FLAG IS NECESSARY, NOT SUFFICIENT. LENS_BATCH_ENABLED=true is an operator saying "I want
// this lane"; it is not evidence that the lane bills. Those are different facts, and the ledger
// only cares about the second. So the gate is the CONJUNCTION: the lane opens when the operator
// asked for it AND a settle hook is actually wired.
//
// That is fail-closed at the mount rather than a warning somebody reads. The failure this repo
// already suffered here is a comment describing the gap ("a nil hook (the default) means today's
// behaviour — a completed job that debits nothing") while the route stayed open: prose cannot
// refuse a request. This can.
//
// When the operator asked and the hook is missing, that is a MISCONFIGURATION and it is logged at
// ERROR — silence would leave them believing the lane is live and billing.
func newBatchReg(wantOn, settleWired bool) batchReg {
	if wantOn && !settleWired {
		slog.Error("batch lane REFUSED: LENS_BATCH_ENABLED is set but no billing settle hook is " +
			"wired (BatchRouter.SetSettleHook has no production caller), so every completed batch " +
			"job would debit nothing while Talyvor pays the provider. The lane stays CLOSED. Wire " +
			"a settle that bills the batch's real consumed tokens before enabling it.")
		return batchReg{on: false}
	}
	return batchReg{on: wantOn}
}

// batchFlagOverride reports the lane's REAL state to the money-flag readout.
//
// newBatchReg can refuse a lane the operator asked for — the flag is necessary and not
// sufficient — and that refusal is taken at wiring time, so *config.Config still holds
// BatchEnabled=true afterwards. Reporting the struct value alone would tell an operator the
// unbilled lane is open when it is closed, which is the exact failure internal/econflags exists
// to prevent, and the same shape as the pooling gate's override next door.
//
// Nil is returned when config and behaviour agree: an override is only meaningful as a
// disagreement, and a "override: same value" entry would be noise on a money surface.
func batchFlagOverride(cfg *config.Config, gate batchReg) func() []econflags.Override {
	return func() []econflags.Override {
		if cfg.BatchEnabled == gate.on {
			return nil
		}
		return []econflags.Override{{
			Name:      "BatchEnabled",
			Effective: gate.on,
			Reason: "LENS_BATCH_ENABLED is set but the lane is CLOSED: no billing settle hook is " +
				"wired in this binary, so every completed batch job would debit nothing while " +
				"Talyvor pays the provider. See newBatchReg in cmd/lens/batch_routes.go.",
		}}
	}
}

func (b batchReg) get(r chi.Router, pattern string, h http.HandlerFunc) {
	if b.on {
		r.Get(pattern, h)
	}
}

func (b batchReg) post(r chi.Router, pattern string, h http.HandlerFunc) {
	if b.on {
		r.Post(pattern, h)
	}
}
