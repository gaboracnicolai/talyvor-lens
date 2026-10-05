package plans

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// B32.12 — the value lens.env.example gives LENS_PLAN_GATES is Nicolai's, the defaults; a malformed one is refused.
func TestLoad_TheEnvExampleIsNicolaisValues(t *testing.T) {
	raw, err := os.ReadFile("../../lens.env.example")
	if err != nil {
		t.Fatal(err)
	}
	var example string
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "# "+Setting+"="); ok {
			example = v
		}
	}
	got, err := Load(func(name string) string { return map[string]string{Setting: example}[name] })
	if err != nil || !reflect.DeepEqual(got, Defaults()) {
		t.Fatalf("lens.env.example's %s loads as %+v, %v; want the defaults %+v", Setting, got, err, Defaults())
	}
	want := map[string]Gates{
		Free:       {Agents: 3, Seats: 1, OwnKeys: OwnKeysNone},
		Team:       {Agents: 25, Seats: 5, OwnKeys: OwnKeysAddOn, LiveMoney: true, SlackTeamsApprovals: true},
		Business:   {Agents: -1, Seats: 25, OwnKeys: OwnKeysIncluded, LiveMoney: true, SlackTeamsApprovals: true, SSO: true, AuditExport: true},
		Enterprise: {Agents: -1, Seats: -1, OwnKeys: OwnKeysIncluded, LiveMoney: true, SlackTeamsApprovals: true, SSO: true, AuditExport: true, Edge: true},
	}
	if !reflect.DeepEqual(Defaults(), want) {
		t.Fatalf("the defaults are %+v, want Nicolai's %+v", Defaults(), want)
	}
	for name, v := range map[string]string{
		"not JSON":      `{`,
		"a plan short":  `{"free":{"agents":3,"seats":1,"own_provider_keys":"none"}}`,
		"an odd plan":   strings.Replace(example, `"free"`, `"gratis"`, 1),
		"agents -2":     strings.Replace(example, `"agents":3`, `"agents":-2`, 1),
		"own keys typo": strings.Replace(example, `"own_provider_keys":"none"`, `"own_provider_keys":"no"`, 1),
		"a typo'd gate": strings.Replace(example, `"sso":false`, `"ssso":false`, 1),
	} {
		if _, err := Load(func(string) string { return v }); err == nil || !strings.Contains(err.Error(), Setting) {
			t.Errorf("%s: Load = %v, want refused naming %s", name, err, Setting)
		}
	}
}
