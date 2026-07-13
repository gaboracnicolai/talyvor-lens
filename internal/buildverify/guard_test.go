package buildverify

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// These proofs run EVERYWHERE (no container runtime needed), so the containment INTENT is verified in CI even
// when the live sandbox tests skip. A containment property must never be "green by absence".

// MINT-FREE: buildverify imports no money/economy package. It compiles code; it moves no value. Step 3 wires
// the verdict to the slash path — buildverify itself must stay disjoint from it.
func TestBuildVerify_ImportGuard_NoMoney(t *testing.T) {
	forbidden := []string{"internal/mining", "internal/economy", "internal/poolroyalty", "internal/provenance"}
	fset := token.NewFileSet()
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		af, err := parser.ParseFile(fset, e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, imp := range af.Imports {
			for _, bad := range forbidden {
				if strings.Contains(imp.Path.Value, bad) {
					t.Errorf("%s imports %s — buildverify must be mint-free", e.Name(), imp.Path.Value)
				}
			}
		}
	}
}

// NEVER RUNS TESTS: the only command Verify runs is `go build`. Asserted on the argv constant AND by scanning
// the package source for any `go test` invocation.
func TestBuildVerify_NeverRunsTests(t *testing.T) {
	if len(goBuildArgv) < 2 || goBuildArgv[0] != "go" || goBuildArgv[1] != "build" {
		t.Fatalf("the build argv must be `go build …`; got %v", goBuildArgv)
	}
	for _, a := range goBuildArgv {
		if strings.Contains(a, "test") {
			t.Errorf("the build argv must never contain 'test'; got %v", goBuildArgv)
		}
	}
	// Source scan: no `"test"` STRING LITERAL in the non-test package files — a `[]string{"go","test",…}`
	// argv would contain it. (We match the quoted literal, not the phrase "go test" in doc comments.)
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, _ := os.ReadFile(e.Name())
		if strings.Contains(string(b), `"test"`) {
			t.Errorf("%s contains a \"test\" string literal — the verifier must NEVER run tests", e.Name())
		}
	}
}

// ENV SCRUB: the container env is an ALLOW-LIST; host secrets are NEVER forwarded. Proven structurally by
// constructing the docker args with dangerous host vars set and asserting none appear.
func TestBuildVerify_EnvAllowlist_NoHostSecrets(t *testing.T) {
	secrets := map[string]string{
		"LENS_K4_SECRET": "leak-lens-xyz",
		"DATABASE_URL":   "postgres://leak",
		"AWS_SECRET_KEY": "leak-aws",
		"GITHUB_TOKEN":   "ghp_leak",
		"OPENAI_API_KEY": "sk-leak",
		"SSH_AUTH_SOCK":  "/leak/agent.sock",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	args := dockerRunArgs("/tmp/src", "img", defaultLimits(), goBuildArgv)
	joined := strings.Join(args, "\x00")
	for k, v := range secrets {
		if strings.Contains(joined, v) {
			t.Errorf("host secret value for %s leaked into the container args", k)
		}
		if strings.Contains(joined, k+"=") {
			t.Errorf("host var %s was forwarded into the container (must be allow-list only)", k)
		}
	}
	// EVERY `-e` value must be an explicit KEY=VALUE from the hermeticity allow-list — NEVER a bare `-e VAR`,
	// which docker resolves to the HOST's value at runtime (an implicit-passthrough leak the value/`KEY=`
	// checks above cannot see).
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-e" {
			ev := args[i+1]
			if !strings.Contains(ev, "=") {
				t.Errorf("bare `-e %s` passthrough forwards the host value — every -e must be KEY=VALUE", ev)
			}
			if !containsStr(containerEnv, ev) {
				t.Errorf("`-e %s` is not in the hermeticity allow-list", ev)
			}
		}
	}
	// The container env must include the hermeticity controls.
	for _, want := range []string{"CGO_ENABLED=0", "GOTOOLCHAIN=local", "GOFLAGS=-mod=vendor", "GOPROXY=off"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing hermeticity control %q in container env", want)
		}
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// CONTAINMENT FLAGS: every hardening control is present in the docker command.
func TestBuildVerify_ContainmentFlags_Present(t *testing.T) {
	args := strings.Join(dockerRunArgs("/tmp/src", "img", defaultLimits(), goBuildArgv), " ")
	for _, must := range []string{
		"--network=none",     // no egress
		"--user 65534:65534", // non-root
		"--security-opt=no-new-privileges",
		"--cap-drop=ALL",
		"--read-only",      // ro root FS
		"noexec",           // tmpfs non-executable
		"--memory 512m",    // mem cap
		"--pids-limit 256", // no fork bomb
		"/tmp/src:/src:ro", // source read-only
	} {
		if !strings.Contains(args, must) {
			t.Errorf("hardened docker command missing %q\n  got: %s", must, args)
		}
	}
	if strings.Contains(args, "--privileged") || strings.Contains(args, "--network=host") {
		t.Error("command contains a containment-breaking flag")
	}
}
