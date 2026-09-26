package config

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// compose_forwarding_test.go — B7.2: every LENS_* variable config.go reads must be FORWARDED by
// docker-compose.yaml's lens service, or it cannot reach the process in production.
//
// `.env` on the server is read only for ${VAR} substitution inside the compose file, so a variable
// config.go reads and the lens service does not list is set in .env, silently dropped, and read as
// unset. That has shipped six times, the last being LENS_SUBSCRIPTION_ALLOWANCE_ULXC: B1.6 was live
// code that granted nothing under compose. The service also loads `env_file: lens.env` (#377), and
// that is where every name in testdata/compose_unforwarded.txt is set — see that file's header.
//
// A RATCHET, NOT ZERO. When this was written config.go read 116 LENS_* names the lens service does
// not forward (testdata/compose_unforwarded.txt). Forwarding them all as ${VAR:-} would set each to
// "" rather than leave it unset, and whether "" means the same as unset is per-variable, so that is
// not a mechanical change. The list is frozen: a NEW read that compose does not forward fails here,
// and a listed name that becomes forwarded (or is no longer read) must leave the list, so it only
// shrinks.

const composeUnforwardedBaseline = "testdata/compose_unforwarded.txt"

// lensServiceBlock returns the lens service's section of the compose file: from `  lens:` up to the
// next top-level service key. A name declared for another service does not reach lens.
func lensServiceBlock(compose string) string {
	start := strings.Index(compose, "\n  lens:\n")
	if start < 0 {
		return ""
	}
	rest := compose[start+len("\n  lens:\n"):]
	if next := regexp.MustCompile(`(?m)^  [a-z][a-z0-9_-]*:\s*$`).FindStringIndex(rest); next != nil {
		rest = rest[:next[0]]
	}
	return rest
}

// composeForwardingGaps compares config.go's reads with the lens service's environment list.
// missing: read, not forwarded, not in the baseline. stale: in the baseline but forwarded or unread.
func composeForwardingGaps(configSrc, compose string, baseline map[string]bool) (read, missing, stale []string) {
	reads := map[string]bool{}
	for _, m := range envRead.FindAllStringSubmatch(configSrc, -1) {
		reads[m[1]] = true
	}
	forwarded := composeDeclaredEnv(lensServiceBlock(compose))
	for n := range reads {
		read = append(read, n)
		if !forwarded[n] && !baseline[n] {
			missing = append(missing, n)
		}
	}
	for n := range baseline {
		if forwarded[n] || !reads[n] {
			stale = append(stale, n)
		}
	}
	sort.Strings(read)
	sort.Strings(missing)
	sort.Strings(stale)
	return read, missing, stale
}

func readComposeBaseline(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(composeUnforwardedBaseline)
	if err != nil {
		t.Fatalf("open baseline: %v", err)
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			out[line] = true
		}
	}
	return out
}

func TestComposeForwardsEveryLensVarConfigReads(t *testing.T) {
	root := filepath.Join("..", "..")
	configSrc, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	composeRaw, err := os.ReadFile(filepath.Join(root, "docker-compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	compose := string(composeRaw)
	baseline := readComposeBaseline(t)

	read, missing, stale := composeForwardingGaps(string(configSrc), compose, baseline)
	if len(read) < envReadFloor {
		t.Fatalf("found only %d LENS_* reads in config.go (floor %d) — the instrument is broken", len(read), envReadFloor)
	}
	for _, n := range missing {
		t.Errorf("config.go reads %s but docker-compose.yaml's lens service does not forward it, so it can "+
			"never be set in production. Add to the lens environment: list:\n      - %s=${%s:-}", n, n, n)
	}
	for _, n := range stale {
		t.Errorf("%s is in %s but is now forwarded or no longer read — delete the line (the list only shrinks)",
			n, composeUnforwardedBaseline)
	}

	// POSITIVE CONTROL: the same check, on this compose file with one real forwarded line removed,
	// must name that variable. Without this a parser that found nothing would pass everything.
	const control = "LENS_SUBSCRIPTION_ALLOWANCE_ULXC"
	line := regexp.MustCompile(`(?m)^\s*-\s*` + control + `=.*\n`)
	if !line.MatchString(lensServiceBlock(compose)) {
		t.Fatalf("control: %s is not forwarded by the lens service — this test's own premise is gone", control)
	}
	_, missingWithout, _ := composeForwardingGaps(string(configSrc), line.ReplaceAllString(compose, ""), baseline)
	if len(missingWithout) != len(missing)+1 || !containsString(missingWithout, control) {
		t.Errorf("control: removing %s from compose was not detected (missing = %v)", control, missingWithout)
	}
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
