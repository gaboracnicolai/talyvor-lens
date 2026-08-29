package main

import "testing"

// scoped_handler_scan_table_test.go — the red-first table for callsOnAliasesOf and
// rawValueReachesResponse (#527).

func TestCallsOnAliasesOf_FollowsTheObjectNotTheSpelling(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"a direct call", "\tpromptManager.Create(ctx, p)\n", 1},
		{"through a one-step alias", "\tpm := promptManager\n\tpm.Create(ctx, p)\n", 1},
		{"through a two-step alias chain", "\ta := promptManager\n\tb := a\n\tb.Create(ctx, p)\n", 1},
		{"no call at all", "\t_ = promptManager\n", 0},
		{"only a COMMENT calls it", "\t// promptManager.Create(ctx, p)\n\t_ = promptManager\n", 0},
		{"the call named inside a STRING", "\tlogger.Debug(\"promptManager.Create(ctx, p)\")\n", 0},
		{"a DIFFERENT object's Create", "\totherManager.Create(ctx, p)\n", 0},
		{"the alias calls a DIFFERENT method", "\tpm := promptManager\n\tpm.Get(ctx, id)\n", 0},
		{"both a direct call and an aliased one", "\tpromptManager.Create(ctx, p)\n\tpm := promptManager\n\tpm.Create(ctx, p)\n", 2},
	} {
		got, err := callsOnAliasesOf("synthetic.go", []byte(wrapMain(tc.body)), "promptManager", "Create")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: calls=%d %v want %d", tc.name, len(got), got, tc.want)
		}
	}
	if _, err := callsOnAliasesOf("broken.go", []byte("package main\nfunc run() { this is not go\n"), "x", "Y"); err == nil {
		t.Error("a parse error was swallowed — an unreadable file would report NO bypass calls")
	}
}

func TestRawValueReachesResponse_FollowsTheValueNotTheShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"written directly", "\twriteJSONOK(w, http.StatusOK, localRouterMulti.List())\n", 1},
		{"through a VARIABLE", "\traw := localRouterMulti.List()\n\twriteJSONOK(w, http.StatusOK, raw)\n", 1},
		{"through a two-step chain", "\ta := localRouterMulti.List()\n\tb := a\n\twriteJSONOK(w, http.StatusOK, b)\n", 1},
		{"wrapped in a map, still the raw value", "\traw := localRouterMulti.List()\n\twriteJSONOK(w, http.StatusOK, map[string]any{\"e\": raw})\n", 1},
		{
			// ⚠ THE LEGITIMATE CALL. The local_models health check COUNTS endpoints and serves
			// none; the first draft of the rule this replaces flagged exactly that.
			name: "counted but never written to a response",
			body: "\tn := len(localRouterMulti.List())\n\t_ = n\n", want: 0,
		},
		{"a PROJECTION is written, not the raw value", "\twriteJSONOK(w, http.StatusOK, project(localRouterMulti))\n", 0},
		{"only a COMMENT writes it", "\t// writeJSONOK(w, http.StatusOK, localRouterMulti.List())\n\t_ = w\n", 0},
		{"a DIFFERENT router's List", "\twriteJSONOK(w, http.StatusOK, otherRouter.List())\n", 0},
	} {
		got, err := rawValueReachesResponse("synthetic.go", []byte(wrapMain(tc.body)), "localRouterMulti", "List", "writeJSONOK")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: response writes carrying the raw list = %d %v, want %d", tc.name, len(got), got, tc.want)
		}
	}
	if _, err := rawValueReachesResponse("broken.go", []byte("package main\nfunc run() { this is not go\n"), "x", "Y", "Z"); err == nil {
		t.Error("a parse error was swallowed — an unreadable file would report NO leak")
	}
}
