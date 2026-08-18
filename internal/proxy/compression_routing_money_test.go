package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
)

// THE PROMPT REWRITER CHOOSES THE MODEL, AND THE MODEL IS THE PRICE.
//
// Two files in this repo state the opposite. internal/workspace/compression_policy.go:
// "no money seam on the serving path reads the rewriter's output ... Turning the
// rewriter off therefore moves no money." compression_billing_test.go, on the same
// table: "Every money seam on this path reads `prompt`, the ORIGINAL."
//
// Both tables enumerate the seams that consume a TOKEN COUNT — the budget gate,
// the LXC gate, the reservation hold, the post-serve len/4 estimate — and all four
// of those really do read the original. The enumeration's population is
// "seams that read a length", and the conclusion drawn from it is about MONEY.
// Those are not the same set, and the gap is the whole finding: on the DELEGATED
// routing path (an auto-route request, or a workspace that opted in to
// cost-optimised routing) the base router is handed compressedPrompt, not prompt.
// It scores that string and picks a cost tier from the score, and the picked model
// replaces the caller's when it ranks cheaper. A model is a rate card. Nothing
// about the token COUNT has to move for the BILL to move.
//
// MEASURED THROUGH THE WIRE, NOT READ. Same caller, same bytes, same requested
// model, one workspace with the 0117 gate closed and one with it open:
//
//	closed → the provider was asked for claude-sonnet-4-6
//	open   → the provider was asked for claude-haiku-4-5
//
// RED FIRST AND OBSERVED RED: the assertion below was first written as the
// sentence those two files ship — that the gate does not move the model — and it
// failed with exactly that pair.
//
// ⚠ WHAT IS FIXED HERE IS THE PROSE, NOT THE BEHAVIOUR, AND THE REASON IS THAT THE
// BEHAVIOUR IS A DECISION. Routing on the rewritten prompt is defensible — it is
// the string the provider will actually receive, and scoring the bytes you do not
// send would be its own inconsistency. Routing on the original is equally
// defensible — a customer who consented to a LOSSLESS COMPRESSION did not thereby
// consent to a model downgrade, and this rewriter's own measured saving is 0.000%
// on 308 corpus prompts, so the trade on that path is a cheaper model in exchange
// for nothing. Picking one silently is a change to what a customer is charged;
// that belongs to whoever owns the price, not to a test. Pinned so the choice has
// to be made deliberately.
//
// ⚠ AND THE CHARGE CLAIM THE OTHER TABLE MAKES SURVIVES INTACT. The billed token
// COUNT still does not move with the gate — compression_billing_test.go measures
// that and stays green. What moves is the RATE those tokens are priced at.

// routingCrossingPrompt is just over 2000 bytes, so len/4 clears the router's
// TokenEstimate>500 threshold, and its double spaces collapse it to just under.
// Everything else about it is deliberately inert to the scorer: no fence, no
// "def "/"func "/"class "/"import ", no maths keyword, no "step by step", no
// "write a"/"story"/"poem". "Explain" is present so RequiresReason is true on BOTH
// strings — the point is a single moving part, not a big delta.
func routingCrossingPrompt() string {
	var b strings.Builder
	b.WriteString("Explain the following configuration in detail:\n")
	for b.Len() < 2010 {
		b.WriteString("alpha  beta  gamma  delta  epsilon  zeta  ")
	}
	return b.String()
}

// lastModel returns the model field of the most recent upstream call — the model
// the provider was actually asked for, which is the rate the charge is priced at.
func (c *capturingUpstream) lastModel(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("upstream received no request at all — the assertion below would be vacuous")
	}
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(c.bodies[len(c.bodies)-1], &m); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, c.bodies[len(c.bodies)-1])
	}
	return m.Model
}

// dispatchCompressModel is dispatchCompress with the requested model under the
// caller's control. The shared helper pins claude-haiku-4-5, which the router
// short-circuits as an explicitly-requested cheap model — it never reaches the
// complexity scorer, so it cannot see this seam at all.
func dispatchCompressModel(t *testing.T, p *Proxy, wsID, model, prompt string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": prompt}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", wsID)
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestCompressionRouting_TheGateSelectsADifferentModelAndThereforeADifferentRate(t *testing.T) {
	probe := routingCrossingPrompt()

	off, upOff, sinkOff := newCompressionProxyWithUpstream(t, workspace.Workspace{
		ID: "ws-route-off", Name: "gate closed", Active: true,
		LoggingPolicy:       workspace.LoggingMetadata,
		CostOptimizeRouting: true,
	}, noUsageUpstream)
	dispatchCompressModel(t, off, "ws-route-off", "claude-opus-4-6", probe)

	on, upOn, sinkOn := newCompressionProxyWithUpstream(t, workspace.Workspace{
		ID: "ws-route-on", Name: "gate open", Active: true,
		LoggingPolicy:       workspace.LoggingMetadata,
		CompressionPolicy:   workspace.CompressionAlways,
		CostOptimizeRouting: true,
	}, noUsageUpstream)
	dispatchCompressModel(t, on, "ws-route-on", "claude-opus-4-6", probe)

	sentOff, sentOn := upOff.lastPrompt(t), upOn.lastPrompt(t)

	// PREMISE — the two requests must actually differ on the wire, or the model
	// comparison below is a statement about one request run twice.
	if sentOff != probe {
		t.Fatalf("premise: the closed gate must forward the original unchanged; got %d bytes, want %d", len(sentOff), len(probe))
	}
	if len(sentOn) >= len(sentOff) {
		t.Fatalf("premise: the open gate must forward a SHORTER rewrite (open=%d closed=%d)", len(sentOn), len(sentOff))
	}

	// PREMISE — EXACTLY ONE SCORE COMPONENT MAY MOVE. Without this the finding could
	// be "the rewriter deleted a keyword", which is a different (and already pinned)
	// defect. Here the rewrite touches nothing but whitespace, and the ONLY thing
	// that changes is the length crossing TokenEstimate>500.
	cOff, cOn := router.AnalyseComplexity(sentOff), router.AnalyseComplexity(sentOn)
	if cOff.HasCode != cOn.HasCode || cOff.HasMath != cOn.HasMath ||
		cOff.HasMultiStep != cOn.HasMultiStep || cOff.IsCreative != cOn.IsCreative ||
		cOff.RequiresReason != cOn.RequiresReason {
		t.Fatalf("premise: only the length may move; keyword components differ\n closed=%+v\n open  =%+v", cOff, cOn)
	}
	if cOff.TokenEstimate <= 500 || cOn.TokenEstimate > 500 {
		t.Fatalf("premise: the rewrite must cross the router's TokenEstimate>500 threshold; closed=%d open=%d", cOff.TokenEstimate, cOn.TokenEstimate)
	}
	if cOff.Score() == cOn.Score() {
		t.Fatalf("premise: the complexity score must move (both %d)", cOff.Score())
	}

	// THE MEASUREMENT — what the provider was asked for.
	modelOff, modelOn := upOff.lastModel(t), upOn.lastModel(t)
	if modelOff == modelOn {
		t.Errorf("the gate did NOT move the model (both %q) — if the router has stopped reading the rewriter's output, this whole file is stale and the two prose corrections it carries need re-measuring, not deleting", modelOff)
	}
	if modelOff != "claude-sonnet-4-6" || modelOn != "claude-haiku-4-5" {
		t.Errorf("upstream model: closed=%q open=%q, pinned at sonnet-4-6 / haiku-4-5", modelOff, modelOn)
	}

	// AND THE SAME PAIR ON THE SPEND ROW, which is where the model becomes a rate.
	// The wire and the invoice are different objects; asserting only the wire would
	// leave "but the charge used the requested model" open.
	spentOff, spentOn := lastSpendModel(t, sinkOff), lastSpendModel(t, sinkOn)
	if spentOff != modelOff || spentOn != modelOn {
		t.Errorf("the spend row's model must be the model actually served: wire(closed=%q open=%q) spend(closed=%q open=%q)",
			modelOff, modelOn, spentOff, spentOn)
	}

	// AND THE RATES ARE NOT EQUAL. Named models differing is not yet money; the
	// catalog is what turns the substitution into a different bill.
	costOff, _ := alerts.CostUSDResolved(spentOff, catalog.PurposeCharge, 1000, 0, 0, 0)
	costOn, _ := alerts.CostUSDResolved(spentOn, catalog.PurposeCharge, 1000, 0, 0, 0)
	if costOff == costOn {
		t.Errorf("the two models price 1000 input tokens identically (%v) — then this substitution moves no money and the finding above is overstated", costOff)
	}
	t.Logf("1000 input tokens: closed(%s)=$%v open(%s)=$%v", spentOff, costOff, spentOn, costOn)
}

// THE MUST-STAY-GREEN COMPANION, AND IT IS NOT DECORATION. The seam is the
// DELEGATED path only. A workspace that did not opt in to cost-optimised routing,
// naming a concrete model, never reaches the base router — so the gate cannot move
// its model however hard the rewriter works. Without this, "compression changes
// your model" would read as a claim about every request, and the honest scope is
// narrower. It also fails if a fix is ever attempted by disabling routing.
func TestCompressionRouting_ANonDelegatedRequestKeepsItsModel(t *testing.T) {
	probe := routingCrossingPrompt()

	off, upOff, _ := newCompressionProxyWithUpstream(t, workspace.Workspace{
		ID: "ws-pinned-off", Name: "gate closed", Active: true,
		LoggingPolicy: workspace.LoggingMetadata,
	}, noUsageUpstream)
	dispatchCompressModel(t, off, "ws-pinned-off", "claude-opus-4-6", probe)

	on, upOn, _ := newCompressionProxyWithUpstream(t, workspace.Workspace{
		ID: "ws-pinned-on", Name: "gate open", Active: true,
		LoggingPolicy:     workspace.LoggingMetadata,
		CompressionPolicy: workspace.CompressionAlways,
	}, noUsageUpstream)
	dispatchCompressModel(t, on, "ws-pinned-on", "claude-opus-4-6", probe)

	// FLOOR — AND IT IS THE SECOND VERSION. The first said only "the open gate must
	// still rewrite something", and a positive control (the space-run collapse
	// removed) showed that passes for free: the final TrimSpace still trims two
	// trailing spaces, so a two-byte rewrite satisfied it while the rewrite that
	// MATTERS was gone. What this test claims is that DELEGATION is the protection,
	// so the floor has to be that the rewrite would otherwise have moved the model —
	// i.e. it still crosses the router's score threshold. Anything weaker lets the
	// test pass because nothing happened.
	scoreOff := router.AnalyseComplexity(upOff.lastPrompt(t)).Score()
	scoreOn := router.AnalyseComplexity(upOn.lastPrompt(t)).Score()
	if scoreOff == scoreOn {
		t.Fatalf("floor: on this path the rewrite must still move the router's score (both %d) — otherwise 'the model did not change' says nothing about delegation", scoreOff)
	}
	if got, want := upOff.lastModel(t), "claude-opus-4-6"; got != want {
		t.Errorf("closed gate, non-delegated: model = %q, want the caller's %q", got, want)
	}
	if got, want := upOn.lastModel(t), "claude-opus-4-6"; got != want {
		t.Errorf("open gate, non-delegated: model = %q, want the caller's %q — the rewriter must not reach a model the caller pinned", got, want)
	}
}

// lastSpendModel reads the model off the most recent captured token_events write.
func lastSpendModel(t *testing.T, s *recordingAlertSink) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.spends) == 0 {
		t.Fatal("no spend row was recorded — the charge-side assertion would be vacuous")
	}
	return s.spends[len(s.spends)-1].model
}
