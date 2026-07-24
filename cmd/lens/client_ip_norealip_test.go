package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoRealIPAnywhere — reintroduction lock for chi's spoofable
// middleware.RealIP (GO-2026-5774 / GO-2026-5775 / GO-2026-5777).
//
// This test is the ONLY guard left. The govulncheck CI gate cannot catch a
// reintroduction: the advisories are marked fixed as of chi v5.3.0, and we
// are on v5.3.1, so the scanner reports nothing no matter how many times the
// function is called. Its body is unchanged at v5.3.1 — it still takes the
// leftmost X-Forwarded-For and still writes it into r.RemoteAddr.
//
// Matches real references via the AST, not a text search: the surrounding
// documentation names the function in prose, and a grep cannot tell a call
// site from a comment.
//
// Use clientIPMiddleware / clientIP instead; see client_ip.go for the
// topology argument.
func TestNoRealIPAnywhere(t *testing.T) {
	const root = "../.."

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		// Comments are dropped: only real code references matter.
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}

		// Resolve the local name chi's middleware package is imported under,
		// so an aliased import cannot slip past.
		pkgName := ""
		for _, imp := range file.Imports {
			if strings.Trim(imp.Path.Value, `"`) != "github.com/go-chi/chi/v5/middleware" {
				continue
			}
			pkgName = "middleware"
			if imp.Name != nil {
				pkgName = imp.Name.Name
			}
		}
		if pkgName == "" || pkgName == "_" {
			return nil
		}

		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "RealIP" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkgName {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s references chi %s.RealIP — it is spoofable and govulncheck will NOT flag it on chi >= v5.3.0; use clientIPMiddleware()/clientIP() instead", rel, pkgName)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
}
