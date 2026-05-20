package compressor

import "context"

type Result struct {
	Compressed string
	SavedRatio float64
}

type Compressor struct{}

func New() *Compressor {
	return &Compressor{}
}

func (c *Compressor) Compress(ctx context.Context, prompt string) (Result, error) {
	_ = ctx
	return Result{Compressed: prompt, SavedRatio: 0}, nil
}
