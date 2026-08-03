package tenant

import (
	"errors"
	"testing"
)

// A SCOPELESS KEY PASSES EVERY SCOPE CHECK.
//
// auth.RequireScope grandfathers an empty scope set — `len(ctx.Scopes) == 0 || ctx.HasScope(scope)`
// — so a key minted with `"scopes": []` satisfies every RequireScope in the system, including the
// proxy gate that lets it spend the workspace's credit.
//
// ⚠ THE GRANDFATHER IS NOT THE BUG AND IS NOT TOUCHED. Old rows predate scopes and must keep
// working; changing RequireScope would break them on upgrade. The bug is that the mint ACCEPTS an
// empty list, so the grandfather — meant for history — is reachable by anyone issuing a key today.
// The door is closed at issuance instead: new keys must name a scope, existing rows are unaffected.
//
// Bounded, and worth saying precisely: a scopeless key is still confined to its own workspace, so
// this is privilege escalation WITHIN a tenant, not across one. What it defeats is the ability to
// issue a restricted key at all — hand someone an "analytics" key and they can also drive
// inference and spend.
func TestCreateAPIKey_RefusesAScopelessKey(t *testing.T) {
	if err := ValidateScopes([]string{}); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("an empty scope list was accepted (err=%v) — the minted key would pass every "+
			"RequireScope check, including the proxy gate that spends the workspace's credit", err)
	}
	if err := ValidateScopes(nil); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("a nil scope list was accepted (err=%v) — same key, same consequence", err)
	}
}

// The converse, so the guard is not simply always-refusing: a real scope still mints.
func TestCreateAPIKey_StillAcceptsRealScopes(t *testing.T) {
	for _, ok := range [][]string{{"proxy"}, {"analytics"}, {"proxy", "admin"}} {
		if err := ValidateScopes(ok); err != nil {
			t.Errorf("ValidateScopes(%v) = %v, want nil", ok, err)
		}
	}
	if err := ValidateScopes([]string{"nonsense"}); !errors.Is(err, ErrInvalidScope) {
		t.Errorf("an unknown scope must still be refused, got %v", err)
	}
}
