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

// miningTypeArgs is the 5-element arg list GetTotalSupply binds.
func miningTypeArgs() []any {
	return []any{
		mining.TypeCacheMine, mining.TypeComputeMine, mining.TypeEmbeddingMine,
		mining.TypeAnnotationMine, mining.TypePatternMine,
	}
}

// expectSupply programmes the three supply queries ComputeFairRate
// triggers: GetTotalSupply (direct), GetTotalSupply (inside
// GetCirculatingSupply), and the burned query.
func expectSupply(mock pgxmock.PgxPoolIface, totalMinted, burned float64) {
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(amount\), 0\)`).
		WithArgs(miningTypeArgs()...).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(totalMinted))
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(amount\), 0\)`).
		WithArgs(miningTypeArgs()...).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(totalMinted))
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(-amount\), 0\) FROM lens_token_ledger WHERE type`).
		WithArgs(mining.TypeBurn).
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

func approxEq(a, b float64) bool {
	d := a - b
	return d < 1e-6 && d > -1e-6
}

// ─── ComputeFairRate ─────────────────────────────

func TestComputeFairRate_DerivesFromBackingAndSupply(t *testing.T) {
	engine, mock := newRateEngineMock(t)
	// totalMinted=1000, burned=0 → circulating=1000.
	// backing = 1000*0.10/1000 = 0.10; R_fair = 0.10/0.10 = 1.0.
	expectSupply(mock, 1000, 0)
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
	expectSupply(mock, 1000, 0)
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
	expectSupply(mock, 1000, 0)
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
	expectSupply(mock, 1000, 500)
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
	expectSupply(mock, 1000, 0)
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
	// rate = 2.0; convert 50 LXC → lensCost = 100.
	expectCurrentRate(mock, 2.0, false)
	mock.ExpectBegin()
	// debit LENS: start at 500 balance.
	mock.ExpectQuery(`INSERT INTO lens_token_balances`).
		WithArgs("ws_c").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(500.0, 500.0, 0.0))
	mock.ExpectExec(`INSERT INTO lens_token_ledger`).
		WithArgs("ws_c", -100.0, 400.0, LENSTypeConvertToLXC, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE lens_token_balances`).
		WithArgs("ws_c", 400.0, 500.0, 100.0).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// credit LXC: start at 0.
	mock.ExpectQuery(`INSERT INTO lxc_balances`).
		WithArgs("ws_c").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_minted", "lifetime_spent"}).
			AddRow(0.0, 0.0, 0.0))
	mock.ExpectExec(`INSERT INTO lxc_ledger`).
		WithArgs("ws_c", 50.0, 50.0, LXCTypeConvertFromLENS, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE lxc_balances`).
		WithArgs("ws_c", 50.0, 50.0, 0.0).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	res, err := store.ConvertLENStoLXC(context.Background(), "ws_c", 50.0)
	if err != nil {
		t.Fatalf("ConvertLENStoLXC: %v", err)
	}
	if !approxEq(res.LXCMinted, 50.0) || !approxEq(res.LENSSpent, 100.0) {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !approxEq(res.NewLXCBalance, 50.0) || !approxEq(res.NewLENSBalance, 400.0) {
		t.Fatalf("unexpected balances: %+v", res)
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
	mock.ExpectQuery(`INSERT INTO lens_token_balances`).
		WithArgs("ws_p").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(50.0, 50.0, 0.0))
	mock.ExpectRollback()
	_, err := store.ConvertLENStoLXC(context.Background(), "ws_p", 50.0)
	if !errors.Is(err, ErrInsufficientLENSFor) {
		t.Fatalf("expected ErrInsufficientLENSFor, got %v", err)
	}
}

func TestConvertLENStoLXC_RejectsBelowMinimum(t *testing.T) {
	store, _, _ := newDualTokenMock(t)
	// No DB expectations — must short-circuit before any query.
	_, err := store.ConvertLENStoLXC(context.Background(), "ws", 0.01)
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
	mock.ExpectQuery(`INSERT INTO lxc_balances`).
		WithArgs("ws_s").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_minted", "lifetime_spent"}).
			AddRow(10.0, 10.0, 0.0))
	mock.ExpectExec(`INSERT INTO lxc_ledger`).
		WithArgs("ws_s", -2.5, 7.5, LXCTypeSpend, "ai call", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE lxc_balances`).
		WithArgs("ws_s", 7.5, 10.0, 2.5).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
	if err := store.SpendLXC(context.Background(), "ws_s", 2.5, "ai call"); err != nil {
		t.Fatalf("SpendLXC: %v", err)
	}
}

func TestSpendLXC_InsufficientBalance(t *testing.T) {
	store, _, mock := newDualTokenMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO lxc_balances`).
		WithArgs("ws_x").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_minted", "lifetime_spent"}).
			AddRow(1.0, 1.0, 0.0))
	mock.ExpectRollback()
	err := store.SpendLXC(context.Background(), "ws_x", 5.0, "ai call")
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
		t.Fatalf("expected 0 for new workspace, got %f", bal)
	}
}

func TestGetLXCSnapshot_ComputesUSDValue(t *testing.T) {
	store, _, mock := newDualTokenMock(t)
	mock.ExpectQuery(`SELECT balance, lifetime_minted, lifetime_spent`).
		WithArgs("ws_v").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_minted", "lifetime_spent"}).
			AddRow(50.0, 80.0, 30.0))
	snap, err := store.GetLXCSnapshot(context.Background(), "ws_v")
	if err != nil {
		t.Fatalf("GetLXCSnapshot: %v", err)
	}
	// 50 LXC × $0.10 = $5.00.
	if !approxEq(snap.USDValue, 5.0) {
		t.Fatalf("expected USD value 5.0, got %f", snap.USDValue)
	}
}
