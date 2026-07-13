package outputverify_test

import (
	"testing"

	"github.com/talyvor/lens/internal/outputverify"
)

// resp builds an OpenAI-shaped completion whose assistant content is `content`.
func resp(content string) []byte {
	return []byte(`{"choices":[{"message":{"role":"assistant","content":` + jsonStr(content) + `}}]}`)
}
func toolResp(name, args string) []byte {
	return []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"name":` + jsonStr(name) + `,"arguments":` + jsonStr(args) + `}}]}}]}`)
}
func jsonStr(s string) string {
	// minimal JSON string encoder for test literals
	out := []byte{'"'}
	for _, r := range []byte(s) {
		switch r {
		case '"', '\\':
			out = append(out, '\\', r)
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, r)
		}
	}
	return string(append(out, '"'))
}

func TestVerify_JSONObject(t *testing.T) {
	req := []byte(`{"model":"m","response_format":{"type":"json_object"}}`)
	if r := outputverify.Verify(req, resp(`{"a":1}`)); r.Verdict != outputverify.VerdictPassed || r.ConstraintKind != outputverify.KindJSONObject {
		t.Errorf("valid JSON must pass; got %+v", r)
	}
	if r := outputverify.Verify(req, resp(`not json at all`)); r.Verdict != outputverify.VerdictFailed || r.Reason != outputverify.ReasonInvalidJSON {
		t.Errorf("non-JSON must fail invalid_json; got %+v", r)
	}
}

func TestVerify_JSONSchema(t *testing.T) {
	req := []byte(`{"response_format":{"type":"json_schema","json_schema":{"schema":{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"number"}},"required":["name","age"]}}}}`)
	if r := outputverify.Verify(req, resp(`{"name":"x","age":30}`)); r.Verdict != outputverify.VerdictPassed {
		t.Errorf("schema-satisfying response must pass; got %+v", r)
	}
	if r := outputverify.Verify(req, resp(`{"name":"x"}`)); r.Verdict != outputverify.VerdictFailed || r.Reason != outputverify.ReasonSchemaViolation {
		t.Errorf("missing required key must fail schema_violation; got %+v", r)
	}
	if r := outputverify.Verify(req, resp(`{"name":123,"age":30}`)); r.Verdict != outputverify.VerdictFailed || r.Reason != outputverify.ReasonSchemaViolation {
		t.Errorf("wrong type must fail schema_violation; got %+v", r)
	}
	if r := outputverify.Verify(req, resp(`totally not json`)); r.Verdict != outputverify.VerdictFailed || r.Reason != outputverify.ReasonInvalidJSON {
		t.Errorf("non-JSON under json_schema must fail invalid_json; got %+v", r)
	}
}

func TestVerify_ToolCall(t *testing.T) {
	req := []byte(`{"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}`)
	if r := outputverify.Verify(req, toolResp("get_weather", `{"city":"Paris"}`)); r.Verdict != outputverify.VerdictPassed || r.ConstraintKind != outputverify.KindToolCall {
		t.Errorf("matching tool args must pass; got %+v", r)
	}
	if r := outputverify.Verify(req, toolResp("get_weather", `{"country":"FR"}`)); r.Verdict != outputverify.VerdictFailed || r.Reason != outputverify.ReasonToolArgMismatch {
		t.Errorf("missing required arg must fail tool_arg_mismatch; got %+v", r)
	}
	if r := outputverify.Verify(req, toolResp("get_weather", `not json`)); r.Verdict != outputverify.VerdictFailed || r.Reason != outputverify.ReasonInvalidJSON {
		t.Errorf("invalid tool args JSON must fail invalid_json; got %+v", r)
	}
	// tools declared but the model produced plain text (no tool call) → unverifiable, NOT failed.
	if r := outputverify.Verify(req, resp("here is the weather")); r.Verdict != outputverify.VerdictUnverifiable {
		t.Errorf("declared tools but no tool call → unverifiable; got %+v", r)
	}
}

// The DEFAULT + majority: no machine-checkable constraint → unverifiable (never a failure).
func TestVerify_NoConstraint_Unverifiable(t *testing.T) {
	for _, req := range [][]byte{
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), // plain chat
		[]byte(`{"response_format":{"type":"text"}}`),                       // explicit free text
		[]byte(`not even json`),                                             // unparseable request
	} {
		if r := outputverify.Verify(req, resp("anything at all")); r.Verdict != outputverify.VerdictUnverifiable {
			t.Errorf("no constraint must be unverifiable (never failed); req=%s got=%+v", req, r)
		}
	}
}

// CRITICAL invariant: unverifiable is NEVER failed_constraint — a bond can never be slashed on "couldn't check".
func TestVerify_UnverifiableIsNeverFailed(t *testing.T) {
	// A response that is not JSON, but under NO declared constraint, must be unverifiable, not failed.
	r := outputverify.Verify([]byte(`{"messages":[]}`), resp("free-form prose, not json"))
	if r.Verdict == outputverify.VerdictFailed {
		t.Fatalf("an unconstrained output must never be failed_constraint; got %+v", r)
	}
	if r.Verdict != outputverify.VerdictUnverifiable {
		t.Errorf("want unverifiable; got %+v", r)
	}
}
