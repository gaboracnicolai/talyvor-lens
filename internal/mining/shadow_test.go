package mining

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Shadow mode: the six unproven mints compute what they WOULD pay, record it, and credit
// NOTHING. These tests hold the two properties that make that safe, and both are about
// STRUCTURE rather than behaviour — because a behavioural test proves today's code does not
// credit, while the requirement is that a shadow row CANNOT become real.

type fakeSink struct {
	calls []shadowCall
	err   error
}

type shadowCall struct {
	ws     string
	typ    string
	amount int64
}

func (f *fakeSink) RecordShadow(_ context.Context, ws, typ string, amt int64, _ map[string]any) error {
	f.calls = append(f.calls, shadowCall{ws, typ, amt})
	return f.err
}

// ── 1. The interception itself ───────────────────────────────────────────────

func TestShadowIntercept_ShadowedTypeRecordsAndRefusesToCredit(t *testing.T) {
	sink := &fakeSink{}
	s := &LedgerStore{}
	s.SetShadowSink(sink, []string{TypeEvalContributionHeld})

	err := s.shadowIntercept(context.Background(), "ws-1", TypeEvalContributionHeld, 4_200, nil)
	if !errors.Is(err, ErrShadowedMint) {
		t.Fatalf("a shadowed mint type must return ErrShadowedMint so the caller does not "+
			"credit; got %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("the hypothetical must be recorded exactly once; got %d calls", len(sink.calls))
	}
	got := sink.calls[0]
	if got.ws != "ws-1" || got.typ != TypeEvalContributionHeld || got.amount != 4_200 {
		t.Errorf("recorded %+v, want ws-1/%s/4200", got, TypeEvalContributionHeld)
	}
}

func TestShadowIntercept_UnshadowedTypePassesThroughUntouched(t *testing.T) {
	sink := &fakeSink{}
	s := &LedgerStore{}
	s.SetShadowSink(sink, []string{TypeEvalContributionHeld})

	// A DIFFERENT mint type must be unaffected — shadow mode must not quietly stop a mint that
	// is supposed to pay.
	if err := s.shadowIntercept(context.Background(), "ws-1", TypePoolRoyaltyHeld, 10, nil); err != nil {
		t.Fatalf("an unshadowed type must pass through with no error; got %v", err)
	}
	if len(sink.calls) != 0 {
		t.Errorf("an unshadowed type must record nothing; got %d calls", len(sink.calls))
	}
}

func TestShadowIntercept_NoSinkIsATotalNoOp(t *testing.T) {
	s := &LedgerStore{}
	// Nothing wired ⇒ shadow mode is off ⇒ every type credits exactly as before.
	for _, ty := range []string{TypeEvalContributionHeld, TypePoolRoyaltyHeld, TypeCacheMine} {
		if err := s.shadowIntercept(context.Background(), "ws-1", ty, 1, nil); err != nil {
			t.Errorf("with no sink wired, %q must pass through; got %v", ty, err)
		}
	}
}

// A failure to RECORD must still refuse to credit. The alternative — falling through to a real
// credit when the observation could not be written — would pay for an unproven mint precisely
// when we lost the ability to see it.
func TestShadowIntercept_RecordFailureStillRefusesToCredit(t *testing.T) {
	sink := &fakeSink{err: errors.New("disk on fire")}
	s := &LedgerStore{}
	s.SetShadowSink(sink, []string{TypeEvalContributionHeld})

	err := s.shadowIntercept(context.Background(), "ws-1", TypeEvalContributionHeld, 99, nil)
	if !errors.Is(err, ErrShadowedMint) {
		t.Fatalf("a record failure must STILL block the credit (fail-closed); got %v", err)
	}
}

// The six named mints, pinned. A seventh added to the shadow set without being listed here is
// fine; one of these SIX silently dropped is not.
func TestShadowSet_CoversTheSixUnprovenMints(t *testing.T) {
	sink := &fakeSink{}
	s := &LedgerStore{}
	s.SetShadowSink(sink, ShadowableMintTypes())

	for _, ty := range []string{
		"receipt_mine_provisional",  // POVIMinting
		TypeAnnotationMine,          // AnnotationMinting
		TypeEvalContributionHeld,    // EvalContribution
		TypeRoutingPredictionHeld,   // RoutingPrediction
		TypeLatencyLocalityHeld,     // LatencyMinting
		TypeConfidentialComputeHeld, // ConfidentialMinting
	} {
		if err := s.shadowIntercept(context.Background(), "ws-1", ty, 1, nil); !errors.Is(err, ErrShadowedMint) {
			t.Errorf("%q must be shadowable — it is one of the six unproven mints; got %v", ty, err)
		}
	}
	// And every shadowable type must be a real mint moment, or shadow mode would be
	// intercepting something that never paid in the first place.
	for _, ty := range ShadowableMintTypes() {
		if !IsMintType(ty) {
			t.Errorf("%q is shadowable but is not a mint-moment type", ty)
		}
	}
}

// ── 2. The structural guarantees ─────────────────────────────────────────────

// THE LOAD-BEARING TEST. A shadow row must be structurally incapable of becoming real, and the
// mechanism is that the sink CANNOT REACH THE LEDGER: its method takes no pgx.Tx, so it cannot
// join the mint transaction, and it is handed no ledger type, so it cannot write one.
//
// This reads the SOURCE rather than exercising behaviour, because the hazard is a property of
// the type — a later refactor that widened the interface would pass every behavioural test.
func TestShadowSink_CannotReachTheLedger(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "shadow.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse shadow.go: %v", err)
	}

	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "ShadowSink" {
			return true
		}
		iface, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			t.Fatal("ShadowSink must be an interface")
		}
		found = true
		for _, m := range iface.Methods.List {
			ft, ok := m.Type.(*ast.FuncType)
			if !ok {
				continue
			}
			for _, p := range ft.Params.List {
				src := typeString(p.Type)
				// pgx.Tx would let the sink write inside the mint transaction — including into
				// lens_token_ledger. Any ledger/store type would let it credit directly.
				for _, banned := range []string{"pgx.Tx", "LedgerStore", "DualTokenStore", "pgx.Conn"} {
					if strings.Contains(src, banned) {
						t.Errorf("ShadowSink takes %q — a shadow sink that can reach the ledger is "+
							"one refactor away from crediting. Keep the interface unable to.", src)
					}
				}
			}
		}
		return false
	})
	if !found {
		t.Fatal("ShadowSink interface not found in shadow.go")
	}
}

// The write-side guard, which is the inverted form of "every reader must exclude shadow rows".
// Twelve call sites aggregate lens_token_ledger; asking each to exclude shadow rows forever is a
// standing obligation that the thirteenth reader will miss. Instead: nothing in the shadow path
// may name the ledger table at all. One site to guard, not N.
func TestShadowMint_NeverTouchesTheTokenLedger(t *testing.T) {
	b, err := os.ReadFile("shadow.go")
	if err != nil {
		t.Fatalf("read shadow.go: %v", err)
	}
	src := string(b)
	// Strip comments so the explanation may NAME the table while the code may not.
	var code strings.Builder
	for _, line := range strings.Split(src, "\n") {
		tl := strings.TrimSpace(line)
		if strings.HasPrefix(tl, "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	for _, banned := range []string{"lens_token_ledger", "lens_token_balances"} {
		if strings.Contains(code.String(), banned) {
			t.Errorf("shadow.go's CODE references %q — a shadow row must never enter the ledger "+
				"or a balance table; that is the one path by which it could become real", banned)
		}
	}
}

// typeString renders a type expression back to source text so the guard above can match on it.
// go/printer rather than a hand-rolled switch: a selector, a pointer, a slice of pointers and a
// qualified generic all render correctly, so the ban cannot be evaded by spelling.
func typeString(e ast.Expr) string {
	var sb strings.Builder
	if err := printer.Fprint(&sb, token.NewFileSet(), e); err != nil {
		return ""
	}
	return sb.String()
}
