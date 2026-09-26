package tare

import "context"

// Reducer is one phase-1 reducer and the kind it accepts. New takes the refusal observer (nil is fine).
type Reducer struct {
	Kind Kind
	New  func(observe func(Refusal)) Reduction
}

// Phase1 is Tare's phase-1 reducers in the order they are tried. Each REFUSES content that is not its
// kind, so the first that shrinks the content wins; prose, and anything none of them recognises, is left
// unchanged. The serve path (internal/proxy tareReduce) and Preview both run this one list.
var Phase1 = []Reducer{
	{KindJSON, func(o func(Refusal)) Reduction { return NewJSONReducer().WithObserver(o) }},
	{KindCode, func(o func(Refusal)) Reduction { return NewGoBodyTrimmer().WithObserver(o) }},
	{KindCode, func(o func(Refusal)) Reduction { return NewTSBodyTrimmer().WithObserver(o) }},
	{KindLog, func(o func(Refusal)) Reduction { return NewLogCollapse().WithObserver(o) }},
}

// PreviewResult is what Tare would do to one piece of content. TokensIn/TokensOut are ESTIMATES
// (EstimateTokens), like every token figure in this package.
type PreviewResult struct {
	Reduced   []byte
	Kind      Kind // the kind whose reducer shrank the content; "" when Tare refused
	TokensIn  int
	TokensOut int
	Refused   bool
	Reasons   []string // why each reducer tried declined, deduped, in order
}

// Preview runs Phase1 on content the way the serve path runs it on a request's newest message — without
// a model call and without the request around it. kind "" tries every reducer; otherwise only those of
// that kind.
func Preview(ctx context.Context, content []byte, kind Kind) (PreviewResult, error) {
	var reasons []string
	seen := map[string]bool{}
	observe := func(r Refusal) {
		if !seen[r.Reason] {
			seen[r.Reason] = true
			reasons = append(reasons, r.Reason)
		}
	}
	for _, r := range Phase1 {
		if kind != "" && r.Kind != kind {
			continue
		}
		reduced, tin, tout, err := r.New(observe).Reduce(ctx, content, r.Kind)
		if err != nil {
			return PreviewResult{}, err
		}
		if len(reduced) < len(content) {
			return PreviewResult{Reduced: reduced, Kind: r.Kind, TokensIn: tin, TokensOut: tout}, nil
		}
	}
	t := EstimateTokens(content)
	return PreviewResult{Reduced: content, TokensIn: t, TokensOut: t, Refused: true, Reasons: reasons}, nil
}
