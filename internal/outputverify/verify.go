package outputverify

import (
	"encoding/json"

	"github.com/talyvor/lens/internal/eval"
)

// Verdict values — a SQL-enforceable enum. `unverifiable` is the DEFAULT and the majority; it means "no
// machine-checkable constraint was declared", NOT a failure. A bond must NEVER be slashable on `unverifiable`
// — that is why it is a distinct enum value, not a nullable bool. Only `failed_constraint` is a violation.
const (
	VerdictPassed       = "passed"
	VerdictFailed       = "failed_constraint"
	VerdictUnverifiable = "unverifiable"
)

// Machine-readable failure reasons (only meaningful when Verdict == failed_constraint).
const (
	ReasonInvalidJSON     = "invalid_json"      // response was required to be JSON and is not parseable
	ReasonSchemaViolation = "schema_violation"  // valid JSON, but a required key is absent or type-incompatible
	ReasonToolArgMismatch = "tool_arg_mismatch" // a tool call's arguments violate the declared parameter schema
)

// Constraint kinds the REQUEST can declare (what we could check).
const (
	KindJSONObject = "json_object" // response_format {"type":"json_object"} — must be valid JSON
	KindJSONSchema = "json_schema" // response_format {"type":"json_schema", ...} — required keys + types
	KindToolCall   = "tool_call"   // tools[] declared — a tool call's args must satisfy its parameter schema
	KindNone       = "none"        // no machine-checkable constraint (the majority)
)

// Result is one intrinsic verdict.
type Result struct {
	Verdict        string // passed | failed_constraint | unverifiable
	Reason         string // set only when failed_constraint
	ConstraintKind string // json_object | json_schema | tool_call | none
}

func unverifiable() Result { return Result{Verdict: VerdictUnverifiable, ConstraintKind: KindNone} }
func passed(kind string) Result {
	return Result{Verdict: VerdictPassed, ConstraintKind: kind}
}
func failed(kind, reason string) Result {
	return Result{Verdict: VerdictFailed, Reason: reason, ConstraintKind: kind}
}

// Verify extracts the machine-checkable constraint the REQUEST declared and validates the RESPONSE against
// it, reusing eval.StaticScore's INTRINSIC methods (json_schema / json.Valid). It compares to NO reference
// answer and to NO other tenant. It is deliberately CONSERVATIVE for a money oracle: it returns
// failed_constraint ONLY on an unambiguous violation; every ambiguity (unparseable request, missing
// response content, unrecognised constraint shape) yields unverifiable, NEVER failed.
func Verify(requestBody, responseBody []byte) Result {
	var req map[string]any
	if err := json.Unmarshal(requestBody, &req); err != nil {
		return unverifiable() // can't read the declared constraint → never a failure
	}

	// (1) tools[] declared + the response actually made a tool call → validate the call's arguments.
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		if r, checked := verifyToolCall(tools, responseBody); checked {
			return r
		}
		// tools declared but the model didn't call one (or we can't read it) → not a violation.
		return unverifiable()
	}

	// (2) response_format constraint.
	rf, ok := req["response_format"].(map[string]any)
	if !ok {
		return unverifiable()
	}
	content, ok := responseText(responseBody)
	if !ok {
		return unverifiable() // no assistant content to check (error/empty/unknown shape) → never a failure
	}
	switch rf["type"] {
	case "json_object":
		if _, _, _ = eval.StaticScore(eval.EvalJSONSchema, "", content); jsonValid(content) {
			return passed(KindJSONObject)
		}
		return failed(KindJSONObject, ReasonInvalidJSON)
	case "json_schema":
		schema := extractJSONSchema(rf)
		if schema == nil {
			return unverifiable() // declared json_schema but we can't read its shape → conservative
		}
		if !jsonValid(content) {
			return failed(KindJSONSchema, ReasonInvalidJSON)
		}
		expected := requiredKeyShape(schema)
		if expected == "" {
			// A schema with no required top-level keys we can pin → only the valid-JSON guarantee holds.
			return passed(KindJSONSchema)
		}
		score, handled, err := eval.StaticScore(eval.EvalJSONSchema, expected, content)
		if err != nil || !handled {
			return unverifiable()
		}
		if score == 1.0 {
			return passed(KindJSONSchema)
		}
		return failed(KindJSONSchema, ReasonSchemaViolation)
	default:
		return unverifiable()
	}
}

// jsonValid reports whether s is a parseable JSON value (reuses the same json.Valid the eval kernel uses).
func jsonValid(s string) bool { return json.Valid([]byte(s)) }

// responseText pulls the assistant text out of an OpenAI-shaped completion (all providers are normalised to
// this shape upstream). ok=false ⇒ no single assistant string is present (error, tool-only, unknown shape).
func responseText(body []byte) (string, bool) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return "", false
	}
	choices, ok := m["choices"].([]any)
	if !ok || len(choices) == 0 {
		return "", false
	}
	c0, ok := choices[0].(map[string]any)
	if !ok {
		return "", false
	}
	msg, ok := c0["message"].(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := msg["content"].(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// verifyToolCall validates the FIRST tool call in the response against the matching declared tool's
// parameter schema. checked=false ⇒ no comparable tool call present (not a violation).
func verifyToolCall(tools []any, body []byte) (Result, bool) {
	name, argsJSON, ok := firstToolCall(body)
	if !ok {
		return Result{}, false
	}
	params := toolParams(tools, name)
	if params == nil {
		return Result{}, false // declared tools but not this one, or no parameter schema → not checkable
	}
	if !jsonValid(argsJSON) {
		return failed(KindToolCall, ReasonInvalidJSON), true
	}
	expected := requiredKeyShape(params)
	if expected == "" {
		return passed(KindToolCall), true
	}
	score, handled, err := eval.StaticScore(eval.EvalJSONSchema, expected, argsJSON)
	if err != nil || !handled {
		return Result{}, false
	}
	if score == 1.0 {
		return passed(KindToolCall), true
	}
	return failed(KindToolCall, ReasonToolArgMismatch), true
}

// firstToolCall returns the name + raw arguments JSON of the first tool call in an OpenAI-shaped response.
func firstToolCall(body []byte) (name, argsJSON string, ok bool) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return "", "", false
	}
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return "", "", false
	}
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) == 0 {
		return "", "", false
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	n, _ := fn["name"].(string)
	a, _ := fn["arguments"].(string)
	if n == "" || a == "" {
		return "", "", false
	}
	return n, a, true
}

// toolParams finds the declared JSON-schema `parameters` object for the tool named `name`.
func toolParams(tools []any, name string) map[string]any {
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			continue
		}
		if n, _ := fn["name"].(string); n != name {
			continue
		}
		if p, ok := fn["parameters"].(map[string]any); ok {
			return p
		}
	}
	return nil
}

// extractJSONSchema pulls the `schema` object out of a response_format {"type":"json_schema","json_schema":{"schema":{...}}}.
func extractJSONSchema(rf map[string]any) map[string]any {
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		return nil
	}
	if s, ok := js["schema"].(map[string]any); ok {
		return s
	}
	return nil
}

// requiredKeyShape translates a JSON-Schema object's REQUIRED top-level keys + their declared types into
// the eval kernel's expected-map shape (key → a typed zero sample), marshalled to JSON. This is a pure
// format ADAPTER — the actual pass/fail decision is eval.scoreJSONSchema (required keys present + type
// compatible). It is deliberately CONSERVATIVE: only top-level `required` keys with a recognised `type` are
// pinned (nested/enum/format constraints are NOT checked), so it never FAILS a response for a constraint it
// didn't actually verify. Returns "" when nothing checkable can be pinned.
func requiredKeyShape(schema map[string]any) string {
	req, _ := schema["required"].([]any)
	props, _ := schema["properties"].(map[string]any)
	if len(req) == 0 || props == nil {
		return ""
	}
	expected := map[string]any{}
	for _, k := range req {
		key, ok := k.(string)
		if !ok {
			continue
		}
		pm, ok := props[key].(map[string]any)
		if !ok {
			continue
		}
		switch pm["type"] {
		case "string":
			expected[key] = ""
		case "number", "integer":
			expected[key] = float64(0)
		case "boolean":
			expected[key] = false
		case "object":
			expected[key] = map[string]any{}
		case "array":
			expected[key] = []any{}
		default:
			// unknown/unpinnable type → require key PRESENCE only (nil matches any present value).
			expected[key] = nil
		}
	}
	if len(expected) == 0 {
		return ""
	}
	b, err := json.Marshal(expected)
	if err != nil {
		return ""
	}
	return string(b)
}
