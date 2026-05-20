package learner

import (
	"context"

	"github.com/nats-io/nats.go"
)

type Event struct {
	Provider    string
	Model       string
	PromptHash  string
	InputTokens int
	Cached      bool
	Compressed  bool
}

type Learner struct {
	nc *nats.Conn
}

func New(nc *nats.Conn) *Learner {
	return &Learner{nc: nc}
}

func (l *Learner) Observe(ctx context.Context, e Event) error {
	_ = ctx
	_ = e
	return nil
}
