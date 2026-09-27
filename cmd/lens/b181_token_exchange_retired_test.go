package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// B18.1 — THE TOKEN EXCHANGE AND TOKEN STAKING ARE NOT SERVED.
//
// Every route main.go registers — through a router verb or through econ.{get,post,del} — is read
// from its AST and replayed into a chi router, and each retired route is requested with the method
// it used to serve: chi answers 404 for a route nobody registered. POVI's stakes, a different
// mechanism, must still be there, or the replay proves nothing.
func TestB181_TokenExchangeAndStakingRoutes404(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	method := map[string]string{
		"get": http.MethodGet, "post": http.MethodPost, "del": http.MethodDelete, // econReg
		"Get": http.MethodGet, "Post": http.MethodPost, "Put": http.MethodPut, "Patch": http.MethodPatch, "Delete": http.MethodDelete,
	}
	r := chi.NewRouter()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel || method[sel.Sel.Name] == "" {
			return true
		}
		for _, arg := range call.Args {
			lit, isLit := arg.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING {
				continue
			}
			if p, uerr := strconv.Unquote(lit.Value); uerr == nil && strings.HasPrefix(p, "/") {
				r.Method(method[sel.Sel.Name], p, http.HandlerFunc(ok))
				n++
			}
			break
		}
		return true
	})
	if n < 100 {
		t.Fatalf("only %d registrations read from main.go — the replay is blind", n)
	}

	code := func(m, path string) int {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(m, path, nil))
		return rec.Code
	}
	for _, rt := range [][2]string{
		{http.MethodGet, "/v1/marketplace/listings"},
		{http.MethodPost, "/v1/marketplace/listings"},
		{http.MethodPost, "/v1/marketplace/listings/l-1/buy"},
		{http.MethodDelete, "/v1/marketplace/listings/l-1"},
		{http.MethodGet, "/v1/marketplace/trades"},
		{http.MethodPost, "/v1/workspaces/ws-1/tokens/stake"},
		{http.MethodPost, "/v1/workspaces/ws-1/tokens/stake/p-1/unstake"},
		{http.MethodGet, "/v1/workspaces/ws-1/tokens/stakes"},
	} {
		if got := code(rt[0], rt[1]); got != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 — the route is still registered in main.go", rt[0], rt[1], got)
		}
	}
	for _, rt := range [][2]string{
		{http.MethodPost, "/v1/workspaces/ws-1/annotate/stake"},
		{http.MethodPost, "/v1/povi/nodes/n-1/stake"},
		{http.MethodGet, "/v1/economy/stats"},
	} {
		if got := code(rt[0], rt[1]); got != http.StatusOK {
			t.Errorf("%s %s = %d, want it still served — B18.1 retires the exchange and token staking only", rt[0], rt[1], got)
		}
	}
}
