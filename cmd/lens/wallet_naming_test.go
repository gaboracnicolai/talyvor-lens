package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// B21.5 — the product is Talyvor Agent Wallets. "Bank" and "banking" are restricted words and
// Talyvor holds no licence (UK: FCA non-objection for a business name; Germany: §39 KWG reserves
// "Bank" for licensed credit institutions, advertising included).
//
// What a developer or user reads must not say it: every string literal in Lens's Go code (OpenAPI
// text, statement titles and CSV headers, MCP tool names and descriptions, error sentences), the
// docs pages, and all of both SDKs. Go identifiers (types, fields) are not read by anyone outside
// and may keep their names. A seller's own "bank account" is not the product's name and is allowed.

// restrictedName is what no user-facing string may contain.
var restrictedName = regexp.MustCompile(`(?i)agent[\s_-]*banks?\b|banking`)

// sdkBank is stricter: the SDKs have no seller payouts, so any "bank" there is the product's old name.
var sdkBank = regexp.MustCompile(`(?i)bank`)

func TestNoBankNameReachesAUser(t *testing.T) {
	for _, s := range []string{"Agent Bank", "the agent bank", "AgentBank", "agent_bank", "banking"} {
		if !restrictedName.MatchString(s) {
			t.Fatalf("restrictedName misses %q — the guard is blind", s)
		}
	}
	if restrictedName.MatchString("the seller's bank account") {
		t.Fatal("restrictedName flags a seller's bank account, which stays")
	}

	root := filepath.Join("..", "..")
	var goFiles, sdkFiles int
	var hits []string

	// Every string literal in non-test Go under cmd/ and internal/.
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			goFiles++
			ast.Inspect(f, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && restrictedName.MatchString(lit.Value) {
					hits = append(hits, fset.Position(lit.Pos()).String()+": "+lit.Value)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	// Both SDKs, whole files: names, docs, examples and tests.
	for _, dir := range []string{"sdk/python", "sdk/typescript"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "node_modules", "dist", "build", ".venv", "__pycache__", ".pytest_cache":
					return filepath.SkipDir
				}
				return nil
			}
			switch filepath.Ext(p) {
			case ".py", ".ts", ".md", ".toml", ".json":
			default:
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			sdkFiles++
			for i, line := range strings.Split(string(b), "\n") {
				if sdkBank.MatchString(line) || restrictedName.MatchString(line) {
					hits = append(hits, p+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	// Docs pages and the README.
	docs, _ := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	for _, p := range append(docs, filepath.Join(root, "README.md")) {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			if restrictedName.MatchString(line) {
				hits = append(hits, p+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}

	if goFiles < 200 || sdkFiles < 10 {
		t.Fatalf("scanned %d Go files and %d SDK files — the walk is not reading the tree", goFiles, sdkFiles)
	}
	if len(hits) > 0 {
		t.Errorf("the product is Talyvor Agent Wallets — \"bank\"/\"banking\" is a restricted word "+
			"and must not reach a developer or user:\n  %s", strings.Join(hits, "\n  "))
	}
}
