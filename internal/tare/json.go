package tare

// json.go — the deterministic, in-house, LOSSLESS JSON compressor (W6.1.1).
//
// ⚠ ONE TRANSFORM, AND IT IS INVERTIBLE BY CONSTRUCTION: an array whose elements are ALL objects
// with the IDENTICAL key set is rewritten from row form to table form.
//
//	[{"file":"a.go","line":1},{"file":"b.go","line":2}]
//	{"~tare":1,"cols":["file","line"],"rows":[["a.go",1],["b.go",2]]}
//
// The saving is the repeated keys — for N objects over K keys, every key after the first row.
// Columns whose value is identical in every row are hoisted once into "const" and dropped from the
// rows entirely.
//
// ⚠ WHY "IDENTICAL KEY SET" AND NOT "UNION WITH NULLS". A union would let a missing key become an
// explicit null, and `{"a":1}` is NOT the same document as `{"a":1,"b":null}` — an agent that
// branches on presence reads them differently. Tabling only exact-shape runs is the difference
// between a lossless transform and one that is lossless "in practice". Mixed shapes are REFUSED.
//
// ⚠ NUMBERS ARE CARRIED AS THEIR ORIGINAL LITERALS. Decoding JSON into `any` makes every number a
// float64, which destroys integers above 2^53 and rewrites 1.0 as 1 — silently lossy on exactly
// the fields an agent reads (ids, counts, hashes). This package decodes with UseNumber() and
// re-encodes json.Number verbatim. TestJSON_BigIntegersAndPreciseDecimalsSurviveExactly is the lock.
//
// ⚠ WHAT IS NOT PRESERVED, STATED PLAINLY: object member ORDER, and insignificant whitespace. JSON
// objects are unordered by RFC 8259, so neither is meaning — but neither is byte-identity, so a
// caller that hashes the raw bytes of a request must hash BEFORE reduction, not after. Prefix
// stability (Phase 1c) depends on that ordering being STABLE, which it is: keys are emitted sorted,
// deterministically. TestJSON_IsDeterministic is the lock.
//
// ⚠ AND IT MUST NEVER GROW THE PAYLOAD. On a short array the table header costs more than the keys
// it removes, so the result is measured and the input returned unchanged when it does not win.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// tableMarker is the key that identifies a tabled array. The "~" prefix is not decoration: it makes
// a collision with a real payload key vanishingly unlikely, and ExpandJSON refuses to treat an
// object as a table unless the marker AND the shape are both right.
const tableMarker = "~tare"

// tableVersion is stamped into every table so a future format change is detectable rather than
// silently misread by an old expander.
const tableVersion = 1

// minTableRows is the smallest run worth tabling. Below it the header cannot pay for itself, and
// the "never grow" check would reject the result anyway — this just avoids the work.
const minTableRows = 2

type JSONReducer struct{ observe func(Refusal) }

func NewJSONReducer() *JSONReducer { return &JSONReducer{} }

// WithObserver registers a sink for refusals. See the note on Reduction: the item's signature has
// nowhere to carry a reason, and a refusal nobody can count is indistinguishable from a reduction
// that achieved nothing.
func (r *JSONReducer) WithObserver(f func(Refusal)) *JSONReducer { r.observe = f; return r }

func (r *JSONReducer) refuse(content []byte, kind Kind, reason string) ([]byte, int, int, error) {
	if r.observe != nil {
		r.observe(Refusal{Kind: kind, Bytes: len(content), Reason: reason})
	}
	t := EstimateTokens(content)
	return content, t, t, nil
}

// Reduce implements Reduction.
func (r *JSONReducer) Reduce(_ context.Context, content []byte, kind Kind) ([]byte, int, int, error) {
	if kind != KindJSON {
		return r.refuse(content, kind, ReasonWrongKind)
	}
	if len(bytes.TrimSpace(content)) == 0 {
		return r.refuse(content, kind, ReasonEmpty)
	}
	doc, err := decodeJSON(content)
	if err != nil {
		return r.refuse(content, kind, ReasonNotJSON)
	}
	transformed, changed := tableDictArrays(doc)
	if !changed {
		return r.refuse(content, kind, ReasonNoDictArray)
	}
	out, err := json.Marshal(transformed)
	if err != nil {
		return r.refuse(content, kind, ReasonReencodeFailed)
	}
	// ⚠ MEASURED, NOT ASSUMED. The header can cost more than the keys it removed.
	if len(out) >= len(content) {
		return r.refuse(content, kind, ReasonNotSmaller)
	}
	return out, EstimateTokens(content), EstimateTokens(out), nil
}

// decodeJSON parses with UseNumber so numeric literals survive byte-for-byte.
func decodeJSON(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	// Trailing content means this was not one document; refuse rather than silently keep the first.
	if d.More() {
		return nil, errors.New("tare: trailing content after the JSON document")
	}
	return v, nil
}

// tableDictArrays walks the document and rewrites every eligible array. It reports whether it
// changed anything, so a document with no eligible array is REFUSED rather than re-encoded — a
// re-encode alone would reorder keys and strip whitespace while saving nothing worth the churn.
func tableDictArrays(v any) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		changed := false
		out := make(map[string]any, len(t))
		for k, val := range t {
			nv, c := tableDictArrays(val)
			out[k] = nv
			changed = changed || c
		}
		return out, changed

	case []any:
		// Depth first: an inner array is tabled before the outer one is judged.
		changed := false
		elems := make([]any, len(t))
		for i, e := range t {
			ne, c := tableDictArrays(e)
			elems[i] = ne
			changed = changed || c
		}
		if tbl, ok := asTable(elems); ok {
			return tbl, true
		}
		return elems, changed

	default:
		return v, false
	}
}

// asTable rewrites a row-form array into table form, or reports that it is not eligible.
func asTable(elems []any) (map[string]any, bool) {
	if len(elems) < minTableRows {
		return nil, false
	}
	var cols []string
	for i, e := range elems {
		m, ok := e.(map[string]any)
		if !ok || len(m) == 0 {
			return nil, false
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			// A payload key colliding with the marker would make the expanded form ambiguous.
			if k == tableMarker {
				return nil, false
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if i == 0 {
			cols = keys
			continue
		}
		if !equalStrings(cols, keys) {
			// ⚠ MIXED SHAPES ARE REFUSED, not unioned. See the file header.
			return nil, false
		}
	}

	// Hoist columns that are identical in every row.
	constCols := map[string]any{}
	for ci, c := range cols {
		_ = ci
		first := elems[0].(map[string]any)[c]
		same := true
		for _, e := range elems[1:] {
			if !sameJSONValue(first, e.(map[string]any)[c]) {
				same = false
				break
			}
		}
		if same {
			constCols[c] = first
		}
	}
	var rowCols []string
	for _, c := range cols {
		if _, isConst := constCols[c]; !isConst {
			rowCols = append(rowCols, c)
		}
	}

	rows := make([]any, 0, len(elems))
	for _, e := range elems {
		m := e.(map[string]any)
		row := make([]any, 0, len(rowCols))
		for _, c := range rowCols {
			row = append(row, m[c])
		}
		rows = append(rows, row)
	}

	out := map[string]any{
		tableMarker: json.Number(fmt.Sprintf("%d", tableVersion)),
		"cols":      toAnySlice(rowCols),
		"rows":      rows,
	}
	if len(constCols) > 0 {
		out["const"] = constCols
	}
	return out, true
}

// ExpandJSON is the inverse. It is EXPORTED because a transform whose inverse is not runnable is a
// transform nobody can check — every test in this package round-trips through it, and a consumer
// of a reduced payload needs it to read one.
func ExpandJSON(b []byte) ([]byte, error) {
	doc, err := decodeJSON(b)
	if err != nil {
		return nil, err
	}
	return json.Marshal(expand(doc))
}

func expand(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if rows, ok := asRows(t); ok {
			return rows
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = expand(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = expand(e)
		}
		return out
	default:
		return v
	}
}

// asRows turns a table back into the array it came from, or reports that this object is not one.
//
// ⚠ EVERY PART OF THE SHAPE IS CHECKED, not just the marker. A payload that happens to carry a
// "~tare" key must not be mangled into an array; the marker is a hint, and the shape is the proof.
func asRows(m map[string]any) ([]any, bool) {
	marker, ok := m[tableMarker]
	if !ok {
		return nil, false
	}
	n, ok := marker.(json.Number)
	if !ok || n.String() != fmt.Sprintf("%d", tableVersion) {
		return nil, false
	}
	rawCols, ok := m["cols"].([]any)
	if !ok {
		return nil, false
	}
	rawRows, ok := m["rows"].([]any)
	if !ok {
		return nil, false
	}
	constCols, _ := m["const"].(map[string]any)
	// An unexpected key means this is not a table this version wrote.
	for k := range m {
		switch k {
		case tableMarker, "cols", "rows", "const":
		default:
			return nil, false
		}
	}
	cols := make([]string, 0, len(rawCols))
	for _, c := range rawCols {
		s, ok := c.(string)
		if !ok {
			return nil, false
		}
		cols = append(cols, s)
	}
	out := make([]any, 0, len(rawRows))
	for _, r := range rawRows {
		vals, ok := r.([]any)
		if !ok || len(vals) != len(cols) {
			return nil, false
		}
		obj := make(map[string]any, len(cols)+len(constCols))
		for k, v := range constCols {
			obj[k] = expand(v)
		}
		for i, c := range cols {
			obj[c] = expand(vals[i])
		}
		out = append(out, obj)
	}
	return out, true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameJSONValue compares two decoded values for constant-column hoisting. It marshals both, which
// is exact for json.Number and stable for maps (Go sorts keys), and it is only ever used to decide
// whether hoisting is SAFE — a false negative costs a little saving, never correctness.
func sameJSONValue(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func toAnySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
