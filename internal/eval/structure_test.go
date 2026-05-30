package eval

import (
	"go/build"
	"testing"
)

// TestEvalHasNoHotPathDependency enforces the ONE property that matters: eval
// is not on the request hot path. It must never import internal/proxy or
// internal/api.
//
// Unlike the forecast/costanomaly structural tests, this one deliberately does
// NOT forbid net/http. Those packages are pure analytics over stored data and
// make no network calls; eval is analytics that REQUIRES model calls — running
// prompts against providers to score them is its whole job, and those calls are
// background/on-demand, never in the request path. Forbidding net/http here
// would assert "eval makes no HTTP call" (false and undesirable), not "eval is
// off the hot path" (the real, enforced property).
func TestEvalHasNoHotPathDependency(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	forbidden := map[string]bool{
		"github.com/talyvor/lens/internal/proxy": true,
		"github.com/talyvor/lens/internal/api":   true,
	}
	for _, imp := range pkg.Imports { // non-test imports only
		if forbidden[imp] {
			t.Errorf("eval must not import %q — eval runs are background/on-demand, off the request hot path", imp)
		}
	}
}
