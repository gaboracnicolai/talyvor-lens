package eval

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Additional, transparent scoring methods layered on top of the original four
// (heuristic, exact, contains, llm_judge). These are deterministic and
// network-free — no model call, no judge opinion — so a case using them is
// fully reproducible.
const (
	// EvalRegex: ExpectedOutput is a regular expression; the case passes
	// (score 1.0) when the response matches it.
	EvalRegex EvalMethod = "regex"
	// EvalJSONSchema: validates the response as JSON. With an empty
	// ExpectedOutput it checks "is valid JSON"; with ExpectedOutput set to a
	// JSON object it additionally requires every key in that object to be
	// present in the response with a type-compatible value (a lightweight,
	// dependency-free required-keys+types schema check).
	EvalJSONSchema EvalMethod = "json_schema"
)

// staticScore scores a response for the deterministic, network-free methods.
// handled=false means the method is not a static one (heuristic / llm_judge),
// so the caller should fall through to the scorer / judge path. A non-nil err
// is a CONFIGURATION error (bad regex, malformed schema) — surfaced to the
// result rather than silently scored 0, so a broken case is visible.
func staticScore(method EvalMethod, expected, response string) (score float64, handled bool, err error) {
	switch method {
	case EvalExact:
		if response == expected {
			return 1.0, true, nil
		}
		return 0, true, nil
	case EvalContains:
		if expected != "" && strings.Contains(response, expected) {
			return 1.0, true, nil
		}
		return 0, true, nil
	case EvalRegex:
		re, cerr := regexp.Compile(expected)
		if cerr != nil {
			return 0, true, fmt.Errorf("eval: invalid regex %q: %w", expected, cerr)
		}
		if re.MatchString(response) {
			return 1.0, true, nil
		}
		return 0, true, nil
	case EvalJSONSchema:
		return scoreJSONSchema(expected, response)
	default:
		return 0, false, nil
	}
}

// scoreJSONSchema implements the json_schema method. Empty schema → valid-JSON
// check. Non-empty schema → required keys present + type-compatible.
func scoreJSONSchema(schema, response string) (float64, bool, error) {
	if strings.TrimSpace(schema) == "" {
		if json.Valid([]byte(response)) {
			return 1.0, true, nil
		}
		return 0, true, nil
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(schema), &want); err != nil {
		return 0, true, fmt.Errorf("eval: malformed json_schema (want a JSON object): %w", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(response), &got); err != nil {
		// Response isn't a JSON object → fails the schema (not a config error).
		return 0, true, nil
	}
	for key, wv := range want {
		gv, ok := got[key]
		if !ok {
			return 0, true, nil
		}
		if !jsonTypeCompatible(wv, gv) {
			return 0, true, nil
		}
	}
	return 1.0, true, nil
}

// jsonTypeCompatible reports whether the response value gv has a JSON type
// compatible with the schema sample value wv. A nil schema value matches any
// present value (key-presence-only requirement).
func jsonTypeCompatible(wv, gv any) bool {
	if wv == nil {
		return true
	}
	return jsonKind(wv) == jsonKind(gv)
}

func jsonKind(v any) string {
	switch v.(type) {
	case bool:
		return "bool"
	case float64, json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}
