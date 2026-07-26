package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func captureLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fn()
	return buf.String()
}

// ⭐ THE CASE THIS EXISTS FOR: a sibling service's gateway secret in Lens's environment. No static
// check in this repo can see it — talyvor-suite's deploy step writes it into the .env that lives in
// the Lens checkout — but the process environment shows it plainly.
func TestEnvHygiene_NamesAnotherServicesSecret(t *testing.T) {
	env := []string{
		"LENS_ANTHROPIC_API_KEY=x",
		"POSTGRES_PASSWORD=p",
		"TRACK_GATEWAY_AUTH_SECRET=super-secret-value",
		"DOCS_GATEWAY_AUTH_SECRET=another-secret-value",
	}
	out := captureLogs(t, func() { logEnvironmentHygieneOf(env) })

	if !strings.Contains(out, "TRACK_GATEWAY_AUTH_SECRET") || !strings.Contains(out, "DOCS_GATEWAY_AUTH_SECRET") {
		t.Errorf("both sibling secrets must be NAMED at boot; got:\n%s", out)
	}
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("credential-shaped foreign variables must be ERROR, not a quiet warn; got:\n%s", out)
	}
	// ⚠ NAMES ONLY. Logging the value would turn a leak into a published leak.
	if strings.Contains(out, "super-secret-value") || strings.Contains(out, "another-secret-value") {
		t.Fatalf("THE VALUE WAS LOGGED — this check would itself become the disclosure:\n%s", out)
	}
}

// A clean environment must stay silent, or the signal is noise and gets ignored.
func TestEnvHygiene_QuietWhenOnlyLensAndAllowedVars(t *testing.T) {
	env := []string{
		"LENS_ANTHROPIC_API_KEY=x", "LENS_ECONOMY_ENABLED=true",
		"POSTGRES_PASSWORD=p", "PATH=/usr/bin", "HOME=/root", "HOSTNAME=lens",
	}
	if out := captureLogs(t, func() { logEnvironmentHygieneOf(env) }); strings.Contains(out, "environment hygiene") {
		t.Errorf("a clean environment must produce no hygiene log; got:\n%s", out)
	}
}

// ⭐ THE INVERTED HAZARD: the default is SAFE and the CHANGE is unsafe, so the warning belongs at the
// boot that made the change — not in a preflight someone has to know to run.
func TestEmbeddingModelWarning_FiresOnlyWhenChangedAndPoolingOn(t *testing.T) {
	const def = "text-embedding-3-small"
	cases := []struct {
		name        string
		model       string
		pooling     bool
		wantWarning bool
	}{
		{"changed + pooling on ⇒ WARN", "text-embedding-ada-002", true, true},
		{"changed + pooling OFF ⇒ silent (no cross-tenant exposure)", "text-embedding-ada-002", false, false},
		{"default + pooling on ⇒ silent (the shipped, measured configuration)", def, true, false},
		{"empty ⇒ silent (config supplies the default)", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureLogs(t, func() {
				warnEmbeddingModelAffectsPoolingMargin(tc.model, def, tc.pooling)
			})
			got := strings.Contains(out, "pooling safety margin")
			if got != tc.wantWarning {
				t.Errorf("warning fired = %v, want %v; got:\n%s", got, tc.wantWarning, out)
			}
			if tc.wantWarning && !strings.Contains(out, "lens poolcheck") {
				t.Errorf("the warning must name the command that MEASURES the margin, or the reader has "+
					"a concern and no action; got:\n%s", out)
			}
		})
	}
}
