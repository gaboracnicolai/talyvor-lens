package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// compose_env_reach_test.go — a config value the compose file does not forward is MUTE: it can be
// set in .env, look correct in every review, and never reach the process.
//
// This has now happened twice, both found by accident. LENS_PROVISION_SECRET was mute, so signup
// 404'd every login while Lens looked healthy. LENS_CACHE_POOLABLE_ENABLED was mute, which is the
// global gate on cross-tenant pooling — set it by hand, run the royalty test, see no mint, and
// there is no way to tell "not implemented" from "not switched on".
//
// WHY A TEST AND NOT A REVIEW HABIT. `.env` is NOT passed into containers. Compose reads it only
// for ${VAR} substitution inside the compose file itself, and this stack declares no `env_file:`
// anywhere — so a variable reaches the process if and only if it appears in a service's
// `environment:` list. Nothing about editing .env tells you that. The gap is invisible from
// inside the process (an unset variable and an unforwarded one are the same empty string), so no
// behavioural test can find it. It is only visible from outside, in the file that forwards.
//
// WHAT THIS GUARDS, AND WHAT IT DELIBERATELY DOES NOT. Lens's config reads ~160 LENS_* variables
// and compose forwards ~28. Most of the difference is fine: feature flags that default off and
// nobody wants on, tuning knobs with sane defaults, alternatives to values compose already sets a
// better way. Requiring all 160 would be noise, and noise is how a guard gets deleted.
//
// So the list below is not "everything", it is everything whose absence is SILENT AND COSTLY. The
// inclusion rule, written down so the next person can decide rather than guess:
//
//	A variable belongs in mustForward if being unable to set it would (a) leave money uncollected,
//	unmetered or unbounded, (b) leave a security control unarmed, or (c) make a shipped capability
//	inert with no error anywhere. "It has a safe default" is NOT an exemption when the default is
//	the off position of the thing you are deploying — that is exactly the pooling case.
var mustForward = map[string]string{
	// ── the money path ────────────────────────────────────────────────────────
	"LENS_CACHE_POOLABLE_ENABLED":       "the GLOBAL gate on cross-tenant pooling. Mute ⇒ pooling never fires no matter how many workspaces opt in, and nothing reports it — the royalty test just shows no mint",
	"LENS_ECONOMY_ENABLED":              "the master economy switch. Mute ⇒ the operator cannot turn the economy off (or back on) at all",
	"LENS_POOL_ROYALTY_MINTING_ENABLED": "the royalty mint itself. Mute ⇒ contributors are never paid and the earning half is inert",
	"LENS_POOL_ROYALTY_SHARE":           "s, the contributor share. Also a SECURITY parameter — the anti-self-dealing property is that farming loses (1-s)*C, so an operator who cannot set it cannot tune it either",
	"LENS_DISTILL_POOLABLE_ENABLED":     "the distill half of pooling consent. Mute ⇒ distill reuse never pools",
	"LENS_MINT_RATE_CAP_LENS_24H":       "the 24h mint cap. Mute ⇒ the cap cannot be tightened in response to abuse",

	// ── customer money in ─────────────────────────────────────────────────────
	"LENS_BILLING_ENABLED":       "Mute ⇒ checkout routes are never registered and a customer cannot buy LXC at all",
	"LENS_STRIPE_SECRET_KEY":     "Mute ⇒ billing cannot charge",
	"LENS_STRIPE_WEBHOOK_SECRET": "Mute ⇒ the webhook cannot verify signatures, so paid credit is never applied",
	"LENS_BILLING_SUCCESS_URL":   "Mute ⇒ a paying customer is returned to the compiled-in default, not this deployment",
	"LENS_BILLING_CANCEL_URL":    "Mute ⇒ same, on the cancel path",
}

// TestComposeForwardsEveryCriticalConfigVar fails when a mustForward variable is not present in
// the lens service's environment list.
func TestComposeForwardsEveryCriticalConfigVar(t *testing.T) {
	compose := readCompose(t)
	lensEnv := lensServiceEnv(t, compose)

	var missing []string
	for name := range mustForward {
		if !forwards(lensEnv, name) {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		t.Errorf("docker-compose.yaml does not forward %s to the lens service — %s.\n"+
			"    Add `- %s=${%s:-}` to the lens service's environment. `.env` alone does NOT reach "+
			"the container: compose reads it only for ${} substitution, and this stack declares no "+
			"env_file.", name, mustForward[name], name, name)
	}
}

// TestCriticalConfigVarsAreActuallyReadByConfig keeps the list honest in the other direction: a
// name that config no longer reads is a stale entry that would quietly pass forever.
func TestCriticalConfigVarsAreActuallyReadByConfig(t *testing.T) {
	raw, err := os.ReadFile("../../internal/config/config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	cfg := string(raw)
	for name := range mustForward {
		if !strings.Contains(cfg, `"`+name+`"`) {
			t.Errorf("mustForward lists %s but internal/config never reads it — the entry is stale "+
				"and is guarding nothing. Remove it, or fix the name.", name)
		}
	}
}

// requiredIsCorrect — variables for which `${VAR:?}` (refuse to start when absent) is the RIGHT
// choice, with the reason. The bar: the stack cannot do its job at all without a value, so
// failing loudly at boot beats starting into a broken state.
var requiredIsCorrect = map[string]string{
	"LENS_DOMAIN": "Caddy's public hostname. Without it there is no site to serve and no " +
		"certificate to obtain, so an empty value is not a degraded mode — it is a broken one.",
}

// TestForwardedVarsUseEmptyDefault — an OPTIONAL capability must be forwarded as `${VAR:-}`, not
// `${VAR:?}`. `:?` makes the whole stack refuse to start when the variable is absent, turning a
// capability a deployment never wanted into a hard boot dependency. That is the same mistake as
// the mute one, pointed the other way: instead of failing silently it fails everything.
//
// The exceptions are listed above with their justification rather than pattern-matched, so
// adding one is a decision somebody writes down.
func TestForwardedVarsUseEmptyDefault(t *testing.T) {
	compose := readCompose(t)
	re := regexp.MustCompile(`\$\{(LENS_[A-Z0-9_]+):\?`)
	for _, m := range re.FindAllStringSubmatch(compose, -1) {
		if _, ok := requiredIsCorrect[m[1]]; ok {
			continue
		}
		t.Errorf("%s is forwarded with `:?` (required). Use `:-` (empty default) so a deployment "+
			"that does not use this capability still boots — or add it to requiredIsCorrect with "+
			"the reason it genuinely cannot start without a value.", m[1])
	}
}

// forwards reports whether the block actually FORWARDS name — i.e. contains a real list entry
// `- NAME=...` — rather than merely mentioning it.
//
// ⚠ This distinction is the whole guard. A `strings.Contains(block, name)` version of this test
// passed while the forwarding line was deleted, because the comment above it explaining why the
// variable matters still contained the name. A guard that its own documentation satisfies is not
// a guard; it is a green light wired to nothing. Same shape as a grep that trips on the doc
// comment describing the thing it bans.
func forwards(block, name string) bool {
	re := regexp.MustCompile(`(?m)^\s*-\s*` + regexp.QuoteMeta(name) + `=`)
	return re.MatchString(block)
}

func readCompose(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../docker-compose.yaml")
	if err != nil {
		t.Fatalf("read docker-compose.yaml: %v", err)
	}
	return string(raw)
}

// lensServiceEnv returns the text of the `lens:` service block. Scoped deliberately: forwarding a
// variable to the one-shot `migrate` service does not help the process that serves traffic.
func lensServiceEnv(t *testing.T, compose string) string {
	t.Helper()
	start := strings.Index(compose, "\n  lens:")
	if start < 0 {
		t.Fatal("no `lens:` service in docker-compose.yaml — this guard has drifted from the file it protects")
	}
	rest := compose[start+1:]
	// The block ends at the next top-level service key (two-space indent, not four).
	end := len(rest)
	for i := 1; i < len(rest)-3; i++ {
		if rest[i] == '\n' && rest[i+1] == ' ' && rest[i+2] == ' ' && rest[i+3] != ' ' && rest[i+3] != '#' {
			end = i
			break
		}
	}
	return rest[:end]
}
