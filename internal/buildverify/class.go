// Package buildverify is Talyvor's SANDBOXED, COMPILE-ONLY build verifier — the first time Talyvor executes
// code it did not write. It compiles a Go source tree inside a hard container (no network, non-root,
// read-only FS, scrubbed env, resource-limited) and returns compiled | compile_failed | not_verifiable.
//
// It NEVER runs `go test` and NEVER runs the target's code — only `go build`, which for a pure-Go
// (CGO_ENABLED=0) tree runs no attacker code at build time. It refuses (not_verifiable) anything outside the
// narrow DETERMINISTIC CLASS, because emitting a WRONG verdict would let step 3 false-slash an honest
// workspace. Refusing is always safe; a wrong verdict is not.
//
// NO PRODUCER is wired to this yet (step 3 wires the attested slash). This package + its proofs are step 2.
// It is mint-free and imports no money package.
package buildverify

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Verdict is the compile-only outcome. not_verifiable is the fail-open default for anything we cannot
// verify DETERMINISTICALLY and SAFELY.
type Verdict string

const (
	Compiled      Verdict = "compiled"
	CompileFailed Verdict = "compile_failed"
	NotVerifiable Verdict = "not_verifiable"
)

// Result carries the verdict + the exact toolchain that produced it. A verdict is only meaningful against a
// NAMED compiler, so Toolchain is always populated for a real verdict (e.g. "go1.25.11").
type Result struct {
	Verdict   Verdict
	Reason    string // why not_verifiable, or a short compile-error summary
	Toolchain string
}

// classify performs the STATIC deterministic-class gate — no code runs here. It returns ("",true) when the
// tree is inside the class, or (reason,false) to refuse. maxGoMinor is the minor version of the pinned
// toolchain (e.g. 25 for go1.25.x); a tree that requires a newer Go than the pinned toolchain is refused
// (the pinned compiler could not build it, so any verdict would be meaningless).
func classify(srcDir string, maxGoMinor int) (reason string, ok bool) {
	gomodPath := filepath.Join(srcDir, "go.mod")
	gomod, err := os.ReadFile(gomodPath)
	if err != nil {
		return "no go.mod at source root (not a buildable module)", false
	}

	// (1) Toolchain pin: the `go` directive must be <= the pinned toolchain. GOTOOLCHAIN=local (set in the
	// sandbox) refuses to DOWNLOAD a newer toolchain, so a newer requirement would just fail to build; we
	// refuse it up front as not_verifiable rather than mis-report it as compile_failed.
	if minor, found := goDirectiveMinor(gomod); found && minor > maxGoMinor {
		return "module requires a newer Go toolchain than the pinned verifier (1." + strconv.Itoa(minor) + " > 1." + strconv.Itoa(maxGoMinor) + ")", false
	}

	// (2) Dependencies must be buildable OFFLINE. Any external require without a vendor/ tree cannot be built
	// with --network=none + GOFLAGS=-mod=vendor + GOPROXY=off. Refuse rather than let the network-blocked
	// build fail and look like a compile error.
	if hasExternalRequire(gomod) {
		if _, err := os.Stat(filepath.Join(srcDir, "vendor", "modules.txt")); err != nil {
			return "module has external dependencies but no vendor/ tree (offline build impossible)", false
		}
	}

	// (3) No cgo. `import "C"` invokes the C toolchain on attacker-controlled C at BUILD TIME (and #cgo
	// directives can pass arbitrary flags to the compiler/linker) — the single biggest build-time
	// code-execution vector. CGO_ENABLED=0 in the sandbox is the hard enforcement (a cgo build simply fails
	// to compile, running no C); this scan upgrades the OUTCOME to an honest not_verifiable.
	if cgo, scanReason := importsCgo(srcDir); cgo {
		return scanReason, false
	}

	return "", true
}

// goDirectiveMinor extracts N from a `go 1.N` / `go 1.N.P` directive.
func goDirectiveMinor(gomod []byte) (minor int, found bool) {
	for _, line := range strings.Split(string(gomod), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "go" {
			parts := strings.Split(f[1], ".")
			if len(parts) >= 2 {
				if m, err := strconv.Atoi(parts[1]); err == nil {
					return m, true
				}
			}
		}
	}
	return 0, false
}

// hasExternalRequire reports whether go.mod requires any external module (a module path always contains a
// dot, e.g. example.com/x; the stdlib is never in require). Conservative: single-line OR block form.
func hasExternalRequire(gomod []byte) bool {
	inBlock := false
	for _, raw := range strings.Split(string(gomod), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "require ("):
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case inBlock && line != "" && !strings.HasPrefix(line, "//"):
			if strings.Contains(strings.Fields(line)[0], ".") {
				return true
			}
		case strings.HasPrefix(line, "require "):
			rest := strings.TrimSpace(strings.TrimPrefix(line, "require"))
			if fields := strings.Fields(rest); len(fields) >= 1 && strings.Contains(fields[0], ".") {
				return true
			}
		}
	}
	return false
}

// importsCgo scans every .go file for an `import "C"`. A file that does not parse is skipped (the build will
// surface it as a compile error, running no C thanks to CGO_ENABLED=0).
func importsCgo(srcDir string) (bool, string) {
	found := false
	fset := token.NewFileSet()
	_ = filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		af, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil
		}
		for _, imp := range af.Imports {
			if imp.Path != nil && imp.Path.Value == `"C"` {
				found = true
			}
		}
		return nil
	})
	if found {
		return true, "module uses cgo (import \"C\") — not in the deterministic, no-target-execution class"
	}
	return false, ""
}
