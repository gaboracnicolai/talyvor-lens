package partners

import (
	"context"
	"testing"
	"time"
)

// B37.2 — a rail's last answers are kept in partner_rails (0237): a restart, or another Lens process, shows them,
// and an older write never undoes a newer one.
func TestRailStore_ARestartKeepsEachRailsLastAnswers(t *testing.T) {
	store := NewRailStore(taxPool(t))
	ctx := context.Background()

	first := NewRegistry(nil)
	first.UseTaxData(noTaxRows{})
	first.Probe(ctx)
	failedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	first.health[ServiceFX].failed = failedAt
	if err := first.keepRails(ctx, store); err != nil {
		t.Fatal(err)
	}
	before := first.Rails()

	restarted := NewRegistry(nil)
	if err := restarted.keepRails(ctx, store); err != nil {
		t.Fatal(err)
	}
	for i, rail := range restarted.Rails() {
		want := before[i]
		if rail.LastSuccess == nil || !rail.LastSuccess.Equal(want.LastSuccess.Truncate(time.Microsecond)) {
			t.Errorf("%s: last success after a restart = %v, want %v", rail.Service, rail.LastSuccess, want.LastSuccess)
		}
	}
	if fx := restarted.Rails()[indexOf(ServiceFX)]; fx.LastFailure == nil || !fx.LastFailure.Equal(failedAt) {
		t.Errorf("fx: last failure after a restart = %v, want %v", fx.LastFailure, failedAt)
	}

	// A process that saw an older failure keeps the stored, newer one.
	stale := NewRegistry(nil)
	stale.health[ServiceFX].failed = failedAt.Add(-time.Hour)
	if err := stale.keepRails(ctx, store); err != nil {
		t.Fatal(err)
	}
	if fx := stale.Rails()[indexOf(ServiceFX)]; fx.LastFailure == nil || !fx.LastFailure.Equal(failedAt) {
		t.Errorf("fx: an older failure replaced a newer one: %v, want %v", fx.LastFailure, failedAt)
	}
}
