package reqtrack_test

import (
	"os"
	"testing"

	"github.com/talyvor/lens/internal/testschema"
)

// TestMain runs this package's real-PG tests in their own schema, lens_pkg_reqtrack, so a parallel
// `go test ./...` on one database cannot collide with another package's tables (internal/testschema).
func TestMain(m *testing.M) {
	testschema.Isolate("lens_pkg_reqtrack")
	os.Exit(m.Run())
}
