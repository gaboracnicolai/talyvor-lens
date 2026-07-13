package buildverify

import (
	"os"
	"path/filepath"
	"testing"
)

// mkmod writes a temp module tree from a path→content map and returns its dir.
func mkmod(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The deterministic class: refuse everything outside it (the false-slash defense).
func TestClassify_DeterministicClass(t *testing.T) {
	const pinnedMinor = 25

	t.Run("valid pure-Go → in class", func(t *testing.T) {
		dir := mkmod(t, map[string]string{
			"go.mod":  "module ok\ngo 1.21\n",
			"main.go": "package main\nfunc main(){}\n",
		})
		if reason, ok := classify(dir, pinnedMinor); !ok {
			t.Errorf("valid pure-Go must be in class; refused: %s", reason)
		}
	})

	t.Run("cgo → refused", func(t *testing.T) {
		dir := mkmod(t, map[string]string{
			"go.mod":  "module cg\ngo 1.21\n",
			"main.go": "package main\n\n// #include <stdlib.h>\nimport \"C\"\nfunc main(){ _ = C.malloc }\n",
		})
		if _, ok := classify(dir, pinnedMinor); ok {
			t.Error("a cgo module must be refused (not_verifiable) — build-time C execution vector")
		}
	})

	t.Run("no go.mod → refused", func(t *testing.T) {
		dir := mkmod(t, map[string]string{"main.go": "package main\nfunc main(){}\n"})
		if _, ok := classify(dir, pinnedMinor); ok {
			t.Error("a tree without go.mod must be refused")
		}
	})

	t.Run("external dep without vendor → refused (offline impossible)", func(t *testing.T) {
		dir := mkmod(t, map[string]string{
			"go.mod":  "module needsdep\ngo 1.21\n\nrequire github.com/pkg/errors v0.9.1\n",
			"main.go": "package main\nimport _ \"github.com/pkg/errors\"\nfunc main(){}\n",
		})
		if _, ok := classify(dir, pinnedMinor); ok {
			t.Error("external require without vendor/ must be refused (can't build offline)")
		}
	})

	t.Run("external dep WITH vendor → in class", func(t *testing.T) {
		dir := mkmod(t, map[string]string{
			"go.mod":             "module hasvendor\ngo 1.21\n\nrequire github.com/pkg/errors v0.9.1\n",
			"main.go":            "package main\nfunc main(){}\n",
			"vendor/modules.txt": "# github.com/pkg/errors v0.9.1\n",
		})
		if reason, ok := classify(dir, pinnedMinor); !ok {
			t.Errorf("vendored deps must be in class; refused: %s", reason)
		}
	})

	t.Run("newer Go than the pinned toolchain → refused", func(t *testing.T) {
		dir := mkmod(t, map[string]string{
			"go.mod":  "module newgo\ngo 1.99\n",
			"main.go": "package main\nfunc main(){}\n",
		})
		if _, ok := classify(dir, pinnedMinor); ok {
			t.Error("a module requiring a newer Go than the pinned toolchain must be refused")
		}
	})
}
