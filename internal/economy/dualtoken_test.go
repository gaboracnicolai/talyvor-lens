package economy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/lens/internal/mining"
)

// pgxNoRows is the sentinel pgxmock returns to simulate an empty
// result set.
func pgxNoRows() error { return pgx.ErrNoRows }

// miningTypeArgs is the arg list GetTotalSupply binds (6 since Stage 2.2
// added pool_royalty to the minted-supply allow-list).
func miningTypeArgs() []any {
	return []any{
		mining.TypeCacheMine, mining.TypeComputeMine, mining.TypeEmbeddingMine,
		mining.TypeAnnotationMine, mining.TypePatternMine, mining.TypePoolRoyalty,
	}
}

// uLENS / uLXC are the µ-unit scale (SEC-2): 1 token = 1e6 µ. Test helpers
// multiply whole-token amounts by these to get the integer µ-counts the store
// now speaks.
const (
	uLENS int64 = 1_000_000
	uLXC  int64 = 1_000_000
)

// expectSupply programmes the three supply queries ComputeFairRate
// triggers: GetTotalSupply (direct), GetTotalSupply (inside
// GetCirculatingSupply), and the burned query. Values are integer µLENS.
func expectSupply(mock pgxmock.PgxPoolIface, totalMinted, burned int64) {
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(amount\), 0\)`).
		WithArgs(miningTypeArgs()...).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(totalMinted))
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(amount\), 0\)`).
		WithArgs(miningTypeArgs()...).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(totalMinted))
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(-amount\), 0\) FROM lens_token_ledger WHERE type`).
		WithArgs(mining.TypeBurn, mining.TypeStakeSlash).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(burned))
}

// expectCurrentRate programmes the conversion_rate_history read.
// Pass noHistory=true to simulate "no rate approved yet".
func expectCurrentRate(mock pgxmock.PgxPoolIface, rate float64, noHistory bool) {
	q := mock.ExpectQuery(`SELECT rate FROM conversion_rate_history ORDER BY created_at DESC LIMIT 1`)
	if noHistory {
		q.WillReturnError(pgxNoRows())
		return
	}
	q.WillReturnRows(pgxmock.NewRows([]string{"rate"}).AddRow(rate))
}

func newRateEngineMock(t *testing.T) (*RateEngine, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	ledger := mining.NewLedgerStoreForTesting(mock)
	return newRateEngine(ledger, mock), mock
}

func newDualTokenMock(t *testing.T) (*DualTokenStore, *RateEngine, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	ledger := mining.NewLedgerStoreForTesting(mock)
	engine := newRateEngine(ledger, mock)
	return newDualTokenStore(ledger, mock, engine), engine, mock
}

// approxEq compares Tier-2 rate floats (still float64 under SEC-2). Conserved
// µ-amounts are int64 and compared with ==.
func approxEq(a, b float64) bool {
	d := a - b
	return d < 1e-6 && d > -1e-6
}

// ─── ComputeFairRate ─────────────────────────────

func TestComputeFairRate_DerivesFromBackingAndSupply(t *testing.T) {
	engine, mock := newRateEngineMock(t)
	// totalMinted=1000, burned=0 → circulating=1000.
	// backing = 1000*0.10/1000 = 0.10; R_fair = 0.10/0.10 = 1.0.
	expectSupply(mock, 1000*uLENS, 0)
	expectCurrentRate(mock, 0, true) // no history → prev = floor 1.0
	comp, err := engine.ComputeFairRate(context.Background())
	if err != nil {
		t.Fatalf("ComputeFairRate: %v", err)
	}
	if !approxEq(comp.FairRate, 1.0) {
		t.Fatalf("expected FairRate 1.0, got %f", comp.FairRate)
	}
	if !approxEq(comp.BackingValue, 0.10) {
		t.Fatalf("expected BackingValue 0.10, got %f", comp.BackingValue)
	}
	if !approxEq(comp.Circulating, 1000) {
		t.Fatalf("expected Circulating 1000, got %f", comp.Circulating)
	}
}

func TestComputeFairRate_AppliesSpread(t *testing.T) {
	engine, mock := newRateEngineMock(t)
	// R_fair = 1.0; R_admin = 1.0 * 1.05 = 1.05 (within band, above floor).
	expectSupply(mock, 1000*uLENS, 0)
	expectCurrentRate(mock, 0, true)
	comp, err := engine.ComputeFairRate(context.Background())
	if err != nil {
		t.Fatalf("ComputeFairRate: %v", err)
	}
	if !approxEq(comp.AdminRate, 1.05) {
		t.Fatalf("expected AdminRate 1.05 (fair × 1.05), got %f", comp.AdminRate)
	}
	if comp.Spread != ConversionSpread {
		t.Fatalf("expected spread %f, got %f", ConversionSpread, comp.Spread)
	}
	if comp.Clamped || comp.Floored {
		t.Fatalf("did not expect clamp/floor, got clamped=%v floored=%v", comp.Clamped, comp.Floored)
	}
}

func TestComputeFairRate_ClampsToBand(t *testing.T) {
	engine, mock := newRateEngineMock(t)
	// prev = 2.0 (from history). R_fair = 1.0 → R_admin pre-clamp 1.05.
	// band = [1.8, 2.2]; 1.05 < 1.8 → clamped up to 1.8.
	expectSupply(mock, 1000*uLENS, 0)
	expectCurrentRate(mock, 2.0, false)
	comp, err := engine.ComputeFairRate(context.Background())
	if err != nil {
		t.Fatalf("ComputeFairRate: %v", err)
	}
	if !comp.Clamped {
		t.Fatal("expected Clamped=true")
	}
	if !approxEq(comp.AdminRate, 1.8) {
		t.Fatalf("expected AdminRate clamped to 1.8, got %f", comp.AdminRate)
	}
}

func TestComputeFairRate_AppliesFloor(t *testing.T) {
	engine, mock := newRateEngineMock(t)
	// totalMinted=1000, burned=500 → circulating=500.
	// R_fair = circulating/totalMinted = 0.5; R_admin pre = 0.525.
	// prev = 0.5 → band [0.45, 0.55]; 0.525 within → not clamped.
	// floor: 0.525 < 1.0 → floored to 1.0.
	expectSupply(mock, 1000*uLENS, 500*uLENS)
	expectCurrentRate(mock, 0.5, false)
	comp, err := engine.ComputeFairRate(context.Background())
	if err != nil {
		t.Fatalf("ComputeFairRate: %v", err)
	}
	if !comp.Floored {
		t.Fatal("expected Floored=true")
	}
	if comp.Clamped {
		t.Fatal("did not expect clamp here")
	}
	if !approxEq(comp.AdminRate, Phase1FloorRate) {
		t.Fatalf("expected AdminRate floored to %f, got %f", Phase1FloorRate, comp.AdminRate)
	}
	if !approxEq(comp.FairRate, 0.5) {
		t.Fatalf("expected FairRate 0.5, got %f", comp.FairRate)
	}
}

func TestComputeFairRate_EmptyEconomyFallsBackToFloorFair(t *testing.T) {
	engine, mock := newRateEngineMock(t)
	// No LENS minted at all → backing undefined → FairRate falls
	// back to the floor (1.0). Spread still applies on top (1.05),
	// and the result never dips below the floor.
	expectSupply(mock, 0, 0)
	expectCurrentRate(mock, 0, true)
	comp, err := engine.ComputeFairRate(context.Background())
	if err != nil {
		t.Fatalf("ComputeFairRate: %v", err)
	}
	if !approxEq(comp.BackingValue, 0) {
		t.Fatalf("expected zero backing for empty economy, got %f", comp.BackingValue)
	}
	if !approxEq(comp.FairRate, Phase1FloorRate) {
		t.Fatalf("expected FairRate to fall back to floor, got %f", comp.FairRate)
	}
	if comp.AdminRate < Phase1FloorRate {
		t.Fatalf("AdminRate must never dip below floor, got %f", comp.AdminRate)
	}
}

// ─── CurrentRate ─────────────────────────────────

func TestCurrentRate_FloorWhenNoHistory(t *testing.T) {
	engine, mock := newRateEngineMock(t)
	expectCurrentRate(mock, 0, true)
	rate, err := engine.CurrentRate(context.Background())
	if err != nil {
		t.Fatalf("CurrentRate: %v", err)
	}
	if !approxEq(rate, Phase1FloorRate) {
		t.Fatalf("expected floor %f, got %f", Phase1FloorRate, rate)
	}
}

func TestCurrentRate_ReturnsLatest(t *testing.T) {
	engine, mock := newRateEngineMock(t)
	expectCurrentRate(mock, 1.42, false)
	rate, err := engine.CurrentRate(context.Background())
	if err != nil {
		t.Fatalf("CurrentRate: %v", err)
	}
	if !approxEq(rate, 1.42) {
		t.Fatalf("expected 1.42, got %f", rate)
	}
}

// ─── ApproveRate ─────────────────────────────────

func TestApproveRate_WritesHistory(t *testing.T) {
	engine, mock := newRateEngineMock(t)
	expectSupply(mock, 1000*uLENS, 0)
	expectCurrentRate(mock, 0, true)
	// History insert with all intermediate values. R_admin=1.05,
	// fair=1.0, backing=0.10, circ=1000, spread=0.05, prev=1.0.
	mock.ExpectExec(`INSERT INTO conversion_rate_history`).
		WithArgs(1.05, 1.0, 0.10, 1000.0, ConversionSpread, Phase1FloorRate, "admin@talyvor").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	comp, err := engine.ApproveRate(context.Background(), "admin@talyvor")
	if err != nil {
		t.Fatalf("ApproveRate: %v", err)
	}
	if !approxEq(comp.AdminRate, 1.05) {
		t.Fatalf("expected AdminRate 1.05, got %f", comp.AdminRate)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ─── ConvertLENStoLXC ────────────────────────────

func TestConvertLENStoLXC_AtomicDebitAndMint(t *testing.T) {
	store, _, mock := newDualTokenMock(t)
	// rate = 2.0; convert 50 LXC → lensCost = 100 LENS. All balances/ledger deltas
	// are integer µ-units (SEC-2): 50 LXC = 50e6 µLXC, 100 LENS = 100e6 µLENS.
	expectCurrentRate(mock, 2.0, false)
	mock.ExpectBegin()
	// debit LENS: ensure row + FOR UPDATE read at 500 LENS balance.
	mock.ExpectExec(`INSERT INTO lens_token_balances`).
		WithArgs("ws_c").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery(`SELECT balance, lifetime_earned, lifetime_spent`).
		WithArgs("ws_c").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(500*uLENS, 500*uLENS, int64(0)))
	mock.ExpectExec(`INSERT INTO lens_token_ledger`).
		WithArgs("ws_c", -100*uLENS, 400*uLENS, LENSTypeConvertToLXC, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE lens_token_balances`).
		WithArgs("ws_c", 400*uLENS, 500*uLENS, 100*uLENS).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// credit LXC: ensure row + FOR UPDATE read at 0.
	mock.ExpectExec(`INSERT INTO lxc_balances`).
		WithArgs("ws_c").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery(`SELECT balance, lifetime_minted, lifetime_spent`).
		WithArgs("ws_c").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_minted", "lifetime_spent"}).
			AddRow(int64(0), int64(0), int64(0)))
	mock.ExpectExec(`INSERT INTO lxc_ledger`).
		WithArgs("ws_c", 50*uLXC, 50*uLXC, LXCTypeConvertFromLENS, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE lxc_balances`).
		WithArgs("ws_c", 50*uLXC, 50*uLXC, int64(0)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	res, err := store.ConvertLENStoLXC(context.Background(), "ws_c", 50*uLXC)
	if err != nil {
		t.Fatalf("ConvertLENStoLXC: %v", err)
	}
	if res.LXCMinted != 50*uLXC || res.LENSSpent != 100*uLENS {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.NewLXCBalance != 50*uLXC || res.NewLENSBalance != 400*uLENS {
		t.Fatalf("unexpected balances: %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestCreditLXC_MintsWithoutBurningLENS — the U18b fiat-purchase credit: LXC is
// minted directly (type=purchase, lifetime_minted += amount) with NO LENS debit.
// The "no LENS burn" property is STRUCTURAL here: the mock declares ZERO
// lens_token_* expectations, so any attempt to touch the LENS schema would fail
// as an unexpected call.
func TestCreditLXC_MintsWithoutBurningLENS(t *testing.T) {
	store, _, mock := newDualTokenMock(t)
	mock.ExpectBegin()
	// ensure row + FOR UPDATE read at balance 20 (minted 20).
	mock.ExpectExec(`INSERT INTO lxc_balances`).
		WithArgs("ws_buy").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery(`SELECT balance, lifetime_minted, lifetime_spent`).
		WithArgs("ws_buy").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_minted", "lifetime_spent"}).
			AddRow(20*uLXC, 20*uLXC, int64(0)))
	// credit 100 LXC: ledger row type=purchase + balance update to 120 (minted 120, spent 0).
	mock.ExpectExec(`INSERT INTO lxc_ledger`).
		WithArgs("ws_buy", 100*uLXC, 120*uLXC, LXCTypePurchase, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE lxc_balances`).
		WithArgs("ws_buy", 120*uLXC, 120*uLXC, int64(0)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	newBal, err := store.CreditLXC(context.Background(), "ws_buy", 100*uLXC, "stripe top-up",
		map[string]interface{}{"usd_cents": 1000})
	if err != nil {
		t.Fatalf("CreditLXC: %v", err)
	}
	if newBal != 120*uLXC {
		t.Fatalf("newBal=%v want %v", newBal, 120*uLXC)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestCreditLXC_RejectsNonPositive — a zero/negative credit is a programming or
// money error; it must be refused before any DB work (no row touched).
func TestCreditLXC_RejectsNonPositive(t *testing.T) {
	store, _, mock := newDualTokenMock(t)
	// No expectations registered → any DB call fails the test.
	for _, amt := range []int64{0, -5} {
		if _, err := store.CreditLXC(context.Background(), "ws_buy", amt, "bad", nil); err == nil {
			t.Errorf("CreditLXC(%v) must error", amt)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestConvertLENStoLXC_InsufficientLENS(t *testing.T) {
	store, _, mock := newDualTokenMock(t)
	expectCurrentRate(mock, 2.0, false)
	mock.ExpectBegin()
	// balance 50 < lensCost 100 → reject.
	mock.ExpectExec(`INSERT INTO lens_token_balances`).
		WithArgs("ws_p").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery(`SELECT balance, lifetime_earned, lifetime_spent`).
		WithArgs("ws_p").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(50*uLENS, 50*uLENS, int64(0)))
	mock.ExpectRollback()
	_, err := store.ConvertLENStoLXC(context.Background(), "ws_p", 50*uLXC)
	if !errors.Is(err, ErrInsufficientLENSFor) {
		t.Fatalf("expected ErrInsufficientLENSFor, got %v", err)
	}
}

func TestConvertLENStoLXC_RejectsBelowMinimum(t *testing.T) {
	store, _, _ := newDualTokenMock(t)
	// No DB expectations — must short-circuit before any query. 0.01 LXC = 10_000
	// µLXC, below MinConversionLXC (100_000 µLXC).
	_, err := store.ConvertLENStoLXC(context.Background(), "ws", 10_000)
	if !errors.Is(err, ErrConversionTooSmall) {
		t.Fatalf("expected ErrConversionTooSmall, got %v", err)
	}
}

// ─── one-way invariant ───────────────────────────

// TestNoReverseConversionPath asserts (via reflection) that the
// DualTokenStore exposes no method that converts LXC back to LENS
// or fiat. The peg's integrity depends on LXC being strictly
// one-way, so we guard it structurally.
func TestNoReverseConversionPath(t *testing.T) {
	typ := reflect.TypeOf(&DualTokenStore{})
	forbidden := []string{
		"ConvertLXCtoLENS", "ConvertLXCToLENS", "LXCtoLENS", "LXCToLENS",
		"RefundLXC", "RedeemLXC", "BurnLXCtoLENS", "Withdraw",
	}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		for _, bad := range forbidden {
			if strings.EqualFold(name, bad) {
				t.Fatalf("forbidden reverse-conversion method present: %s", name)
			}
		}
		// Heuristic: any method that mentions both LXC and LENS and
		// also "to"/"refund"/"redeem" beyond the sanctioned
		// ConvertLENStoLXC is suspicious.
		lower := strings.ToLower(name)
		if lower != "convertlenstolxc" &&
			strings.Contains(lower, "lxc") &&
			(strings.Contains(lower, "refund") || strings.Contains(lower, "redeem") || strings.Contains(lower, "tolens")) {
			t.Fatalf("suspicious reverse-conversion method present: %s", name)
		}
	}
}

// ─── SpendLXC ────────────────────────────────────

func TestSpendLXC_DebitsBalance(t *testing.T) {
	store, _, mock := newDualTokenMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO lxc_balances`).
		WithArgs("ws_s").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery(`SELECT balance, lifetime_minted, lifetime_spent`).
		WithArgs("ws_s").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_minted", "lifetime_spent"}).
			AddRow(10*uLXC, 10*uLXC, int64(0)))
	mock.ExpectExec(`INSERT INTO lxc_ledger`).
		WithArgs("ws_s", -25*uLXC/10, 75*uLXC/10, LXCTypeSpend, "ai call", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE lxc_balances`).
		WithArgs("ws_s", 75*uLXC/10, 10*uLXC, 25*uLXC/10).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
	if err := store.SpendLXC(context.Background(), "ws_s", 25*uLXC/10, "ai call"); err != nil {
		t.Fatalf("SpendLXC: %v", err)
	}
}

func TestSpendLXC_InsufficientBalance(t *testing.T) {
	store, _, mock := newDualTokenMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO lxc_balances`).
		WithArgs("ws_x").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery(`SELECT balance, lifetime_minted, lifetime_spent`).
		WithArgs("ws_x").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_minted", "lifetime_spent"}).
			AddRow(1*uLXC, 1*uLXC, int64(0)))
	mock.ExpectRollback()
	err := store.SpendLXC(context.Background(), "ws_x", 5*uLXC, "ai call")
	if !errors.Is(err, ErrInsufficientLXC) {
		t.Fatalf("expected ErrInsufficientLXC, got %v", err)
	}
}

// ─── GetLXCBalance ───────────────────────────────

func TestGetLXCBalance_ZeroForNew(t *testing.T) {
	store, _, mock := newDualTokenMock(t)
	mock.ExpectQuery(`SELECT balance FROM lxc_balances WHERE workspace_id`).
		WithArgs("ws_new").
		WillReturnError(pgxNoRows())
	bal, err := store.GetLXCBalance(context.Background(), "ws_new")
	if err != nil {
		t.Fatalf("GetLXCBalance: %v", err)
	}
	if bal != 0 {
		t.Fatalf("expected 0 for new workspace, got %d", bal)
	}
}

func TestGetLXCSnapshot_ComputesUSDValue(t *testing.T) {
	store, _, mock := newDualTokenMock(t)
	mock.ExpectQuery(`SELECT balance, lifetime_minted, lifetime_spent`).
		WithArgs("ws_v").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_minted", "lifetime_spent"}).
			AddRow(50*uLXC, 80*uLXC, 30*uLXC))
	snap, err := store.GetLXCSnapshot(context.Background(), "ws_v")
	if err != nil {
		t.Fatalf("GetLXCSnapshot: %v", err)
	}
	// USD value is the balance at the fixed peg — derived from the const, never
	// hardcoded: floor(50e6 µLXC × 0.10) µUSD = 5_000_000 µUSD ($5). This is the
	// figure the #182 fiat panel shows.
	want := mining.MulFloor(50*uLXC, LXCUSDValue)
	if snap.USDValue != want {
		t.Fatalf("USD value = %d, want %d µUSD (50 × LXCUSDValue)", snap.USDValue, want)
	}
}
