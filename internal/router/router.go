package router

import "context"

type Decision struct {
	Provider string
	Model    string
	Reason   string
}

type ModelRouter struct{}

func New() *ModelRouter {
	return &ModelRouter{}
}

func (r *ModelRouter) Route(ctx context.Context, provider, requestedModel, prompt string) (Decision, error) {
	_ = ctx
	_ = prompt
	return Decision{
		Provider: provider,
		Model:    requestedModel,
		Reason:   "passthrough",
	}, nil
}
