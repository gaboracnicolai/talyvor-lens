package dbjson

import "testing"

func TestJSONB_Value(t *testing.T) {
	cases := []struct {
		name string
		in   JSONB
		want any
	}{
		{"populated", JSONB(`{"k":"v"}`), `{"k":"v"}`},
		{"nil", nil, "{}"},
		{"empty", JSONB(``), "{}"},
		{"literal null", JSONB(`null`), "{}"}, // json.Marshal(nil map) == "null"
		{"array", JSONB(`[1,2]`), `[1,2]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.in.Value()
			if err != nil {
				t.Fatalf("Value() error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Value() = %#v, want %#v", got, tc.want)
			}
			// Value must always be a string (text-encoded on the wire), never
			// []byte (which would be inferred as bytea under SimpleProtocol).
			if _, ok := got.(string); !ok {
				t.Fatalf("Value() returned %T, want string (text encoding is the #133 fix)", got)
			}
		})
	}
}

func TestMarshal(t *testing.T) {
	j, err := Marshal(map[string]any{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(j) != `{"a":1}` {
		t.Fatalf("Marshal = %q, want %q", string(j), `{"a":1}`)
	}
	// nil round-trips to {} through Value.
	jn, err := Marshal(map[string]any(nil))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := jn.Value(); v != "{}" {
		t.Fatalf("Marshal(nil).Value() = %v, want {}", v)
	}
}
