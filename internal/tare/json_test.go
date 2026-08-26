package tare_test

// json_test.go — W6.1.1: the deterministic JSON compressor.
//
// ⚠ LOSSLESS IS THE WHOLE POINT AND IT IS THE FIRST ASSERTION IN EVERY TEST. W6.1.1 records why:
// "THE PREVIOUS COMPRESSOR REPHRASED CONTENT AND CORRUPTED PROMPTS — 'when to use \"in order to\"
// versus \"to\"' became \"'to' versus 'to'\". That is the failure this whole layer exists to not
// repeat." So the round-trip check runs on EVERY input this file uses, including the ones the
// reducer refuses, and it compares the DECODED DOCUMENTS rather than the bytes.
//
// ⚠ WHY DOCUMENTS AND NOT BYTES. JSON object member order carries no meaning (RFC 8259: "an object
// is an unordered collection"), and Go's encoder emits map keys sorted, so a byte comparison would
// fail on a transform that changed nothing at all. What must survive is the VALUE. The one place
// bytes DO matter is numbers — see the big-number test, which is why this package decodes with
// UseNumber() instead of letting every number become a float64.

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/talyvor/lens/internal/tare"
)

// decodeExact parses with UseNumber so 12345678901234567890 stays that, rather than becoming
// 1.2345678901234567e+19.
func decodeExact(t *testing.T, b []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decode %q: %v", truncate(b), err)
	}
	return v
}

func truncate(b []byte) string {
	if len(b) > 120 {
		return string(b[:120]) + "…"
	}
	return string(b)
}

// mustRoundTrip is the losslessness assertion. It expands the reduced form and requires the result
// to be the SAME DOCUMENT as the input.
func mustRoundTrip(t *testing.T, original, reduced []byte) {
	t.Helper()
	restored, err := tare.ExpandJSON(reduced)
	if err != nil {
		t.Fatalf("the reduced form does not expand: %v\nreduced: %s", err, truncate(reduced))
	}
	want := decodeExact(t, original)
	got := decodeExact(t, restored)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("LOSSY. The expanded document differs from the original.\n original: %s\n restored: %s",
			truncate(original), truncate(restored))
	}
}

func reduce(t *testing.T, in []byte) (out []byte, tin, tout int) {
	t.Helper()
	out, tin, tout, err := tare.NewJSONReducer().Reduce(context.Background(), in, tare.KindJSON)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	return out, tin, tout
}

// ⚠ THE HEADLINE. An array of same-shaped objects is the shape a tool output actually has, and the
// repeated keys are the dead weight.
func TestJSON_TablesAnArrayOfSameShapedObjects(t *testing.T) {
	in := []byte(`[{"file":"a.go","line":1,"severity":"warn"},{"file":"b.go","line":2,"severity":"warn"},{"file":"c.go","line":3,"severity":"error"}]`)
	out, tin, tout := reduce(t, in)

	mustRoundTrip(t, in, out)
	if len(out) >= len(in) {
		t.Fatalf("no reduction: %d bytes in, %d out. Three objects share three keys; the repeated "+
			"keys are exactly what this transform exists to remove", len(in), len(out))
	}
	// ⚠ THE TOKEN ASSERTIONS ARE HERE BECAUSE CONTROL H6 FOUND THEM MISSING. The original check was
	// only `tout > tin` — which a constant-zero estimator satisfies trivially, so the estimate was
	// effectively unasserted. Both ends are pinned now: a non-empty input estimates non-zero, and a
	// reduction that saved bytes must show up in the estimate too.
	if tin <= 0 {
		t.Fatalf("token estimate for %d bytes of input is %d — an estimator that reports nothing "+
			"makes every saving this layer reports meaningless", len(in), tin)
	}
	if tout >= tin {
		t.Fatalf("bytes fell %d -> %d but the token estimate went %d -> %d. The estimate is derived "+
			"from length, so it must move with it", len(in), len(out), tin, tout)
	}
	t.Logf("%d -> %d bytes (%.1f%%)", len(in), len(out), 100*float64(len(in)-len(out))/float64(len(in)))
}

// ⚠ A CONSTANT COLUMN IS HOISTED, and it is still one document afterwards.
func TestJSON_HoistsAColumnThatIsConstantAcrossEveryRow(t *testing.T) {
	in := []byte(`[{"repo":"talyvor-lens","n":1},{"repo":"talyvor-lens","n":2},{"repo":"talyvor-lens","n":3}]`)
	out, _, _ := reduce(t, in)
	mustRoundTrip(t, in, out)
	if len(out) >= len(in) {
		t.Fatalf("no reduction on a constant column: %d -> %d", len(in), len(out))
	}
}

// ⚠ THE REFUSALS. Each is a valid outcome and each must come back BYTE-IDENTICAL with a nil error.
func TestJSON_RefusesAndReturnsTheInputUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, in, wantReason string
		kind                 tare.Kind
	}{
		{"not json", `{oh no`, tare.ReasonNotJSON, tare.KindJSON},
		{"empty", ``, tare.ReasonEmpty, tare.KindJSON},
		{"wrong kind", `[{"a":1},{"a":2}]`, tare.ReasonWrongKind, tare.KindProse},
		{"no dict array", `{"a":1,"b":[1,2,3]}`, tare.ReasonNoDictArray, tare.KindJSON},
		{"objects differ in shape", `[{"a":1},{"b":2}]`, tare.ReasonNoDictArray, tare.KindJSON},
		{"array of scalars", `[1,2,3,4,5]`, tare.ReasonNoDictArray, tare.KindJSON},
		// A one-element array is not a RUN of same-shaped objects, so it is refused for shape
		// rather than for size. (I first expected ReasonNotSmaller here; the implementation is
		// the more accurate of the two and the test moved.)
		{"single object", `[{"a":1,"b":2}]`, tare.ReasonNoDictArray, tare.KindJSON},
		// A genuine not-smaller: two rows over one short key, where the table header costs more
		// than the single repeated key it removes.
		{"table would grow the payload", `[{"a":1},{"a":2}]`, tare.ReasonNotSmaller, tare.KindJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got tare.Refusal
			r := tare.NewJSONReducer().WithObserver(func(f tare.Refusal) { got = f })
			out, tin, tout, err := r.Reduce(context.Background(), []byte(tc.in), tc.kind)
			if err != nil {
				t.Fatalf("a refusal must not be an error: %v", err)
			}
			if !bytes.Equal(out, []byte(tc.in)) {
				t.Fatalf("a refusal must return the input UNCHANGED.\n in: %q\nout: %q", tc.in, out)
			}
			if tin != tout {
				t.Fatalf("a refusal changed the token estimate: %d -> %d", tin, tout)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("refusal reason = %q, want %q. The reason is the whole point of the "+
					"observer: a refusal nobody can count is indistinguishable from a reduction "+
					"that achieved nothing", got.Reason, tc.wantReason)
			}
		})
	}
}

// ⚠ THE NUMBER TEST, AND IT IS THE ONE MOST LIKELY TO HAVE BEEN GOT WRONG. Decoding JSON into
// `any` turns every number into a float64, which silently destroys integers above 2^53 and rewrites
// 1.0 as 1. A compressor that did that would be LOSSY on exactly the fields an agent reads —
// ids, counts, hashes.
func TestJSON_BigIntegersAndPreciseDecimalsSurviveExactly(t *testing.T) {
	in := []byte(`[{"id":12345678901234567890,"ratio":0.1000000000000000055511151231257827,"n":1.0},` +
		`{"id":98765432109876543210,"ratio":2.00,"n":3.000}]`)
	out, _, _ := reduce(t, in)
	mustRoundTrip(t, in, out)

	for _, needle := range []string{"12345678901234567890", "98765432109876543210", "1.0", "3.000", "2.00"} {
		if !bytes.Contains(out, []byte(needle)) {
			t.Fatalf("the literal %q is not in the reduced form — the number was reformatted, which "+
				"is a silent loss on an id or a hash.\nreduced: %s", needle, truncate(out))
		}
	}
}

// ⚠ NOTHING IS DROPPED, INCLUDING THE THINGS W6.1.1 NAMES. The item says KEEP errors, outliers and
// first-and-last elements.
//
// ⚠ UNDER A LOSSLESS TRANSFORM THAT INSTRUCTION IS VACUOUSLY SATISFIED — nothing is dropped at all,
// so "keep the errors" cannot fail. It is asserted anyway, and said out loud here, because the
// moment any elision is added in a later phase this test stops being vacuous and starts being the
// guard the item asked for. A constraint that is only true by accident is worth pinning before the
// accident ends.
func TestJSON_KeepsEveryElementIncludingErrorsOutliersFirstAndLast(t *testing.T) {
	in := []byte(`[{"i":0,"status":"ok","note":"FIRST"},{"i":1,"status":"ok","note":"x"},` +
		`{"i":2,"status":"error","note":"the failure an agent is looking for"},` +
		`{"i":3,"status":"ok","note":"x"},{"i":4,"status":"ok","note":"LAST"}]`)
	out, _, _ := reduce(t, in)
	mustRoundTrip(t, in, out)

	for _, needle := range []string{"FIRST", "LAST", "the failure an agent is looking for", "error"} {
		if !bytes.Contains(out, []byte(needle)) {
			t.Fatalf("%q is gone from the reduced form — this transform must drop NOTHING", needle)
		}
	}
	restored, err := tare.ExpandJSON(out)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(restored, &rows); err != nil {
		t.Fatalf("the expanded form is not the original array shape: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("element count = %d, want 5 — an element was dropped", len(rows))
	}
}

// ⚠ DETERMINISTIC: the same input reduces to the same bytes, every time. A reduction that varies
// run to run cannot be cached, cannot be diffed, and cannot be held to a measured saving.
func TestJSON_IsDeterministic(t *testing.T) {
	// ⚠ NOTE THE KEY ORDER: b,a,c then c,a,b then a,b,c. These are the SAME SHAPE — JSON objects are
	// unordered — and matching them requires comparing key SETS, not key sequences.
	// ⚠ THE KEYS ARE LONG ENOUGH THAT THE TABLE PAYS. The first version used a/b/c and the table
	// header cost more than the three one-character keys it removed, so the reducer REFUSED and
	// this test had been asserting, twenty times over, that a no-op is stable. The added
	// "something was reduced" assertion is what surfaced it.
	in := []byte(`[{"betaField":2,"alphaField":1,"gammaField":3},{"gammaField":6,"alphaField":4,"betaField":5},{"alphaField":7,"betaField":8,"gammaField":9}]`)
	first, _, _ := reduce(t, in)

	// ⚠ THIS ASSERTION EXISTS BECAUSE CONTROL H5 EXPOSED THE TEST AS VACUOUS WITHOUT IT. "Twenty
	// runs agree" is trivially satisfied by a reducer that REFUSES every time — refusal returns the
	// input, which is perfectly stable. So determinism has to be asserted over an actual reduction.
	//
	// ⚠ AND H5 CORRECTED WHAT THE KEY SORT IS FOR. Removing it does not make the OUTPUT vary:
	// json.Marshal sorts map keys and `cols` is a slice built in sorted order, so byte-stability
	// survives. What breaks is SHAPE MATCHING — element 0's map-iteration order stops equalling
	// element 1's, every array is judged mixed-shape, and the reducer silently reduces nothing.
	if len(first) >= len(in) {
		t.Fatalf("nothing was reduced (%d -> %d), so 'the same bytes twenty times' asserts only that "+
			"refusal is stable. These three objects are the same shape with their keys written in "+
			"three different orders; if that is no longer matched, key-set comparison has become "+
			"order-SENSITIVE", len(in), len(first))
	}
	for i := 0; i < 20; i++ {
		again, _, _ := reduce(t, in)
		if !bytes.Equal(first, again) {
			t.Fatalf("run %d produced different bytes:\n%s\n%s", i, truncate(first), truncate(again))
		}
	}
	mustRoundTrip(t, in, first)
}

// ⚠ IT NEVER RETURNS SOMETHING BIGGER. A "reduction" that grows the payload costs money instead of
// saving it, and on a short array the table header can easily exceed the keys it removes.
func TestJSON_NeverReturnsSomethingLargerThanTheInput(t *testing.T) {
	for _, in := range []string{
		`[{"a":1,"b":2}]`,
		`[{"averyveryverylongkeyname":1},{"averyveryverylongkeyname":2}]`,
		`[{"a":1},{"a":2}]`,
		`[{"a":{"deep":{"nested":[1,2,3]}}},{"a":{"deep":{"nested":[4,5,6]}}}]`,
	} {
		out, _, _ := reduce(t, []byte(in))
		if len(out) > len(in) {
			t.Fatalf("GREW: %d -> %d for %s", len(in), len(out), in)
		}
		mustRoundTrip(t, []byte(in), out)
	}
}

// A nested array-of-objects deep inside a document is still the shape worth tabling.
func TestJSON_TablesANestedArrayOfObjects(t *testing.T) {
	in := []byte(`{"tool":"grep","matches":[{"path":"x.go","line":10,"text":"foo"},` +
		`{"path":"y.go","line":20,"text":"bar"},{"path":"z.go","line":30,"text":"baz"}]}`)
	out, _, _ := reduce(t, in)
	mustRoundTrip(t, in, out)
	if len(out) >= len(in) {
		t.Fatalf("a nested dict-array was not tabled: %d -> %d", len(in), len(out))
	}
}
