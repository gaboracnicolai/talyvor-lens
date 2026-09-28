package workspace

import "fmt"

// Audience selects which workspaces an operator aggregate counts (B17.7). Synthetic workspaces (B17.1)
// send real traffic through real paths, so without this every admin total would move whenever the test
// harness runs. The default is AudienceReal; the harness reads its own numbers with AudienceSynthetic.
type Audience int

const (
	// AudienceReal counts every workspace that is not synthetic — what an operator reads by default.
	AudienceReal Audience = iota
	// AudienceSynthetic counts synthetic workspaces only — the harness's own view of its traffic.
	AudienceSynthetic
)

// AudienceQueryParam is the query parameter every operator aggregate reads: absent means real
// workspaces only, "only" means synthetic workspaces only.
const AudienceQueryParam = "synthetic"

// ParseAudience reads the ?synthetic= value. Anything other than "" or "only" is refused rather than
// guessed, so a typo can never quietly return the wrong population.
func ParseAudience(v string) (Audience, error) {
	switch v {
	case "":
		return AudienceReal, nil
	case "only":
		return AudienceSynthetic, nil
	}
	return AudienceReal, fmt.Errorf("synthetic=%q: the only accepted value is \"only\"", v)
}

// String is the audience's name in a response body.
func (a Audience) String() string {
	if a == AudienceSynthetic {
		return "synthetic"
	}
	return "real"
}

// Includes reports whether ws belongs to the audience.
func (a Audience) Includes(ws *Workspace) bool {
	return ws != nil && ws.Synthetic == (a == AudienceSynthetic)
}

// SQL returns a predicate over col (a workspace id column, e.g. "te.workspace_id") that is true for rows
// belonging to the audience. col must be a trusted column expression, never input. It is written with
// EXISTS rather than IN so a row whose workspace has no workspaces row (the legacy 'default' id) still
// counts as real, exactly as it did before synthetic workspaces existed.
func (a Audience) SQL(col string) string {
	exists := "EXISTS (SELECT 1 FROM workspaces aud_ws WHERE aud_ws.id = " + col + " AND aud_ws.synthetic)"
	if a == AudienceSynthetic {
		return exists
	}
	return "NOT " + exists
}
