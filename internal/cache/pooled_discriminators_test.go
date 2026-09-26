package cache

import (
	"testing"

	"github.com/talyvor/lens/internal/discriminator"
	"github.com/talyvor/lens/internal/poolsafety"
)

// corpusPrompts is every prompt in the committed poolsafety corpora, deduped.
func corpusPrompts() []string {
	seen := map[string]bool{}
	var out []string
	add := func(ps ...string) {
		for _, p := range ps {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	for _, l := range poolsafety.ByTraffic() {
		for _, p := range append(append([]poolsafety.RephrasePair{}, l.Rephrase...), l.Danger...) {
			add(p.A, p.B)
		}
	}
	for _, p := range poolsafety.Corpus() {
		add(p.Full(p.A), p.Full(p.B))
	}
	return out
}

// B9.5 — a pooled row's discriminators must be the ones GetPooled computes for the same question.
// The write is handed the marker-prefixed key; computed on that, "Can I enable SSO for Okta?" stored
// caps:sso|propn:can|propn:okta while the read computed caps:sso|propn:okta, so it never matched.
func TestPooledDiscriminators_WriteEqualsReadForEveryCorpusPrompt(t *testing.T) {
	prompts := corpusPrompts()
	if len(prompts) < 100 {
		t.Fatalf("only %d corpus prompts — the corpora did not load", len(prompts))
	}
	mismatched := 0
	for _, p := range prompts {
		write := pooledDiscriminators(PooledPromptKey(p))
		read := string(discriminator.Canon(p))
		if write != read {
			mismatched++
			if mismatched <= 5 {
				t.Errorf("%q: write %q, read %q", p, write, read)
			}
		}
	}
	if mismatched > 0 {
		t.Errorf("%d of %d corpus prompts store discriminators their own read can never match", mismatched, len(prompts))
	}
	if got := pooledDiscriminators(PooledPromptKey("Can I enable SSO for Okta?")); got != string(discriminator.Canon("Can I enable SSO for Okta?")) {
		t.Errorf("the item's example stores %q", got)
	}
}
