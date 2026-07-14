package mining

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// KE-2: the REDUCE-ONLY Keel drift haircut in reputationBondedAmount (real PG, reuses repBondHarness).
// Reputation is seeded HIGH (f(R)=1.0) so the reputation factor is neutral and each case isolates the drift
// haircut. Together these pin the money-path invariants: reduce-only, bounded by HaircutFloor, NEVER increases
// a mint, FAIL-OPEN on a read error, and byte-identical when the seam is unwired. The seam is FAKED here (the
// keel_findings query is proven in internal/royaltyhaircut) so this file tests only the clamp/apply logic.

// (1) A hardened drift (factor 0.5) HALVES the bonded mint; the factor is recorded in metadata.
func TestDriftHaircut_HardenedReduces_Bounded_Integration(t *testing.T) {
	pool, ledger := repBondHarness(t, true, "wsD")
	ctx := context.Background()
	seedR(t, pool, "wsD", 0.7) // f(R)=1.0 — isolate the haircut
	ledger.SetDriftHaircut(func(_ context.Context, _ pgx.Tx, _ string) (float64, error) { return 0.5, nil })

	meta := map[string]interface{}{}
	if err := ledger.Credit(ctx, "wsD", micro(10), rcptType, "d", meta); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if b := bal(t, pool, "wsD"); b != micro(5) {
		t.Errorf("balance %v, want 5 (base 10 × haircut 0.5)", b)
	}
	if meta["drift_haircut_factor"] != 0.5 {
		t.Errorf("metadata drift_haircut_factor = %v, want 0.5", meta["drift_haircut_factor"])
	}
}

// (2) A factor > 1.0 is CLAMPED to 1.0 — the haircut can NEVER increase a mint (monotone-decreasing).
func TestDriftHaircut_NeverIncreases_ClampAboveOne_Integration(t *testing.T) {
	pool, ledger := repBondHarness(t, true, "wsU")
	ctx := context.Background()
	seedR(t, pool, "wsU", 0.7)
	ledger.SetDriftHaircut(func(_ context.Context, _ pgx.Tx, _ string) (float64, error) { return 2.0, nil })

	if err := ledger.Credit(ctx, "wsU", micro(10), rcptType, "d", map[string]interface{}{}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if b := bal(t, pool, "wsU"); b != micro(10) {
		t.Errorf("balance %v, want 10 (a haircut can never INCREASE a mint)", b)
	}
}

// (3) A factor below the floor is CLAMPED UP to HaircutFloor — a drifter earns LESS, never nothing.
func TestDriftHaircut_FlooredNeverBelowHaircutFloor_Integration(t *testing.T) {
	pool, ledger := repBondHarness(t, true, "wsF")
	ctx := context.Background()
	seedR(t, pool, "wsF", 0.7)
	ledger.SetDriftHaircut(func(_ context.Context, _ pgx.Tx, _ string) (float64, error) { return 0.01, nil })

	if err := ledger.Credit(ctx, "wsF", micro(10), rcptType, "d", map[string]interface{}{}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	want := MulFloor(micro(10), HaircutFloor)
	if b := bal(t, pool, "wsF"); b != want {
		t.Errorf("balance %v, want %v (floored at HaircutFloor %v — never zero)", b, want, HaircutFloor)
	}
}

// (4) A read error → NO haircut (fail-open: a false penalty is worse than an open flank).
func TestDriftHaircut_FailOpenOnError_Integration(t *testing.T) {
	pool, ledger := repBondHarness(t, true, "wsE")
	ctx := context.Background()
	seedR(t, pool, "wsE", 0.7)
	ledger.SetDriftHaircut(func(_ context.Context, _ pgx.Tx, _ string) (float64, error) {
		return 0, errors.New("keel unavailable")
	})

	if err := ledger.Credit(ctx, "wsE", micro(10), rcptType, "d", map[string]interface{}{}); err != nil {
		t.Fatalf("mint must still succeed (fail-open): %v", err)
	}
	if b := bal(t, pool, "wsE"); b != micro(10) {
		t.Errorf("balance %v, want 10 (fail-open: a haircut read error applies NO haircut)", b)
	}
}

// (5) An unwired (nil) seam is a total no-op — the mint is byte-identical to before KE-2.
func TestDriftHaircut_NilSeam_ByteIdentical_Integration(t *testing.T) {
	pool, ledger := repBondHarness(t, true, "wsN")
	ctx := context.Background()
	seedR(t, pool, "wsN", 0.7)
	// no SetDriftHaircut → nil seam

	if err := ledger.Credit(ctx, "wsN", micro(10), rcptType, "d", map[string]interface{}{}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if b := bal(t, pool, "wsN"); b != micro(10) {
		t.Errorf("balance %v, want 10 (nil haircut seam = byte-identical mint)", b)
	}
}
