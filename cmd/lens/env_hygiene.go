package main

import (
	"log/slog"
	"os"
	"sort"
	"strings"
)

// ENVIRONMENT HYGIENE — the check that sees what the repository cannot.
//
// ┌─ WHY THIS EXISTS, AND WHY IT IS AT RUNTIME ─────────────────────────────────────────────────────┐
// │ docker-compose.yaml forwards an env file wholesale. Whether that leaks depends on what is IN    │
// │ the file on the box, and no test in this repository can know that:                              │
// │                                                                                                  │
// │   · compose_env_mechanism_test.go reads .env.example / .env.production.example — the DOCUMENTED │
// │     shape, in this repo.                                                                        │
// │   · The hazard is written by a step in ANOTHER repository. talyvor-suite's deploy/README.md     │
// │     (470-476) tells the operator to append TRACK_GATEWAY_AUTH_SECRET and                        │
// │     DOCS_GATEWAY_AUTH_SECRET to the .env in the Lens checkout, because track-docs.compose.yaml  │
// │     runs in this compose project.                                                               │
// │                                                                                                  │
// │ So a green static check meant "nothing unexpected is DOCUMENTED here", and was read as "nothing │
// │ unexpected is PRESENT there". Adjacent to the claim, not the claim — and unfixable statically    │
// │ without this repo reading another repo's deploy prose, which is worse than the problem.          │
// │                                                                                                  │
// │ ⚠ THE PROCESS ENVIRONMENT IS THE ARTIFACT. At boot Lens can simply look at what it was actually │
// │ given. That is not a proxy for the deployed shape; it IS the deployed shape.                    │
// └─────────────────────────────────────────────────────────────────────────────────────────────────┘
//
// This never blocks boot. An unexpected variable is a hygiene signal, not a fault — the operator may
// have a good reason, and refusing to start over a stray env var would be a worse failure than the
// one being reported.

// envHygieneAllowed are variables a Lens container legitimately holds. Everything else in the
// process environment gets named at boot.
//
// The rule for adding one: it must be something Lens ITSELF reads or that the runtime requires. A
// variable belonging to a sibling service does NOT go here — that is the case this check exists to
// surface, and silencing it here would convert a finding into a permanent blind spot.
var envHygieneAllowed = map[string]bool{
	// Container/runtime basics.
	"PATH": true, "HOME": true, "HOSTNAME": true, "TERM": true, "PWD": true, "SHLVL": true,
	"TZ": true, "LANG": true, "LC_ALL": true, "GODEBUG": true, "GOMAXPROCS": true, "GOMEMLIMIT": true,
	"SSL_CERT_DIR": true, "SSL_CERT_FILE": true,
	// Interpolated into LENS_DATABASE_URL by compose; this process already holds the value.
	"POSTGRES_PASSWORD": true,
	// Standard cloud/proxy conventions Lens's own HTTP clients honour.
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
	"AWS_REGION": true, "AWS_DEFAULT_REGION": true, "AWS_ACCESS_KEY_ID": true,
	"AWS_SECRET_ACCESS_KEY": true, "AWS_SESSION_TOKEN": true,
}

// logEnvironmentHygiene names every variable in this process's environment that is neither a LENS_*
// setting nor on the allow-list above.
//
// ⚠ NAMES ONLY, NEVER VALUES. The whole point is that some of these may be another service's
// secrets; logging them would turn a leak into a published leak.
func logEnvironmentHygiene() { logEnvironmentHygieneOf(os.Environ()) }

// logEnvironmentHygieneOf takes the environment rather than reading it, so a test can hand it an
// exact one. The wrapper above is the only caller that touches the process.
//
// (The first version read os.Environ() directly and was untestable for the QUIET case: a Go test
// process carries dozens of unrelated variables, so "clean environment stays silent" could not be
// expressed. A check whose no-alarm path cannot be tested is half a check.)
func logEnvironmentHygieneOf(environ []string) {
	var unexpected []string
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		if strings.HasPrefix(name, "LENS_") || envHygieneAllowed[name] {
			continue
		}
		unexpected = append(unexpected, name)
	}
	if len(unexpected) == 0 {
		return
	}
	sort.Strings(unexpected)

	// A name ending in SECRET/KEY/TOKEN/PASSWORD that is not ours is the specific hazard, so it is
	// called out separately rather than buried in a list.
	var credentialish []string
	for _, n := range unexpected {
		for _, suffix := range []string{"SECRET", "KEY", "TOKEN", "PASSWORD", "CREDENTIALS"} {
			if strings.HasSuffix(n, suffix) {
				credentialish = append(credentialish, n)
				break
			}
		}
	}

	if len(credentialish) > 0 {
		slog.Error("environment hygiene: this container holds CREDENTIAL-SHAPED variables that are not "+
			"Lens's. If they belong to another service, an env file is being forwarded too broadly — a "+
			"Lens compromise or crash dump would expose them. Check docker-compose.yaml's env_file: it "+
			"should forward lens.env (LENS_* only), never the project .env, which the deploy procedure "+
			"also uses for sibling services' gateway secrets.",
			slog.String("variables", strings.Join(credentialish, ",")),
			slog.Int("total_unexpected", len(unexpected)))
		return
	}
	slog.Warn("environment hygiene: variables present that Lens does not read and does not expect. "+
		"Harmless in itself, but it means an env file is being forwarded more broadly than intended.",
		slog.String("variables", strings.Join(unexpected, ",")))
}

// warnEmbeddingModelAffectsPoolingMargin fires when the embedder has been changed away from the
// default while cross-tenant pooling is on.
//
// ⚠ THE DANGER HERE IS INVERTED FROM THE USUAL CONFIG HAZARD, which is why the warning belongs here
// and not only in a preflight. Elsewhere a variable is dangerous because its DEFAULT is the failure
// position and an operator cannot change it. Here the default (text-embedding-3-small) is SAFE and
// the CHANGE is what is unsafe: the cross-tenant safety margin that `lens poolcheck` measures depends
// on the configured embedder, and a weaker model consumes much of it. An operator swapping the model
// to cut embedding cost has no reason to connect that to a pooling safety property.
//
// So the guard belongs where the change is made. `lens poolcheck` is opt-in — someone has to know to
// run it — whereas this fires on the boot of the deployment that actually made the change.
//
// It does NOT block boot and does NOT judge the model: a different embedder may be entirely fine, and
// poolcheck is what measures it. This only ensures nobody arrives at that state silently.
func warnEmbeddingModelAffectsPoolingMargin(model, defaultModel string, poolingEnabled bool) {
	if !poolingEnabled || model == "" || model == defaultModel {
		return
	}
	slog.Warn("the embedding model has been changed while cross-tenant pooling is ENABLED. The "+
		"pooling safety margin is a property of the configured embedder, not a constant — a weaker "+
		"model narrows it, and one workspace's answer being served to another is what the margin "+
		"protects. Run `lens poolcheck` against this configuration before trusting it.",
		slog.String("embedding_model", model),
		slog.String("default_model", defaultModel),
		slog.String("verify_with", "lens poolcheck"))
}
