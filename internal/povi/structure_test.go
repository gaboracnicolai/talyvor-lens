package povi

import (
	"go/build"
	"testing"
)

// TestPOVIHasNoHotPathDependency keeps the PoVI substrate independent of the
// request hot path. Receipt verification + recording happen on the
// network-node-served (compute-mining) accounting path, never inside the proxy
// request flow for ordinary requests — so internal/povi must not import
// internal/proxy or internal/api. (It legitimately depends on internal/metrics
// and pgx; only the hot-path packages are forbidden.)
func TestPOVIHasNoHotPathDependency(t *testing.T) {
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
			t.Errorf("povi must not import %q — the receipt substrate stays off the request hot path", imp)
		}
	}
}
