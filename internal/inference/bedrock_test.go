package inference

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Relocated from internal/proxy/bedrock_test.go (PR-3b A′) — the bedrock UNIT tests move with their funcs.
// Byte-identical except the package line + the now-exported call names (modelToBedrockID →
// ModelToBedrockID, translateToBedrockFormat → TranslateToBedrockFormat, translateFromBedrockFormat →
// TranslateFromBedrockFormat, signRequest → SignRequest). The TestHandleBedrock_* handler tests stay in
// package proxy, byte-identical.

func TestModelToBedrockID_MapsClaudeSonnet46Correctly(t *testing.T) {
	id, ok := ModelToBedrockID("claude-sonnet-4-6")
	if !ok {
		t.Fatal("claude-sonnet-4-6 should be supported")
	}
	if id != "anthropic.claude-sonnet-4-6-20251101-v1:0" {
		t.Errorf("got %q, want anthropic.claude-sonnet-4-6-20251101-v1:0", id)
	}
}

func TestModelToBedrockID_UnknownModelReturnsFalse(t *testing.T) {
	if _, ok := ModelToBedrockID("gpt-4"); ok {
		t.Error("gpt-4 should NOT be supported on Bedrock")
	}
	if _, ok := ModelToBedrockID("gemini-2.5-pro"); ok {
		t.Error("gemini-2.5-pro should NOT be supported on Bedrock")
	}
	if _, ok := ModelToBedrockID(""); ok {
		t.Error("empty model must not map")
	}
}

func TestTranslateToBedrockFormat_RemovesModelField(t *testing.T) {
	in := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := TranslateToBedrockFormat(in)
	if err != nil {
		t.Fatalf("translateToBedrockFormat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := got["model"]; present {
		t.Errorf("Bedrock body still contains 'model' field: %s", out)
	}
}

func TestTranslateToBedrockFormat_AddsAnthropicVersion(t *testing.T) {
	in := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	out, _ := TranslateToBedrockFormat(in)
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["anthropic_version"] != "bedrock-2023-05-31" {
		t.Errorf("anthropic_version = %v, want bedrock-2023-05-31", got["anthropic_version"])
	}
}

func TestTranslateFromBedrockFormat_ReturnsOpenAIShape(t *testing.T) {
	in := []byte(`{"content":[{"type":"text","text":"hi from claude"}],"role":"assistant"}`)
	out, err := TranslateFromBedrockFormat(in, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("translateFromBedrockFormat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion (client should see OpenAI shape)", got["object"])
	}
	choices, _ := got["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in translated response: %v", got)
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if !strings.Contains(msg["content"].(string), "hi from claude") {
		t.Errorf("content lost in translation: %v", msg["content"])
	}
}

func TestSignRequest_AddsAuthorizationHeader(t *testing.T) {
	cfg := BedrockConfig{
		Region: "us-east-1", AccessKeyID: "AKIA-TEST", SecretAccessKey: "SECRET",
	}
	req, _ := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-sonnet-4-6-20251101-v1:0/invoke",
		strings.NewReader(`{"messages":[]}`))
	if err := SignRequest(req, cfg); err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization missing SigV4 prefix: %q", auth)
	}
	if !strings.Contains(auth, "Credential=AKIA-TEST/") {
		t.Errorf("Authorization missing credential: %q", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=host;x-amz-date") {
		t.Errorf("Authorization missing signed headers: %q", auth)
	}
	if !strings.Contains(auth, "Signature=") {
		t.Errorf("Authorization missing signature: %q", auth)
	}
}

func TestSignRequest_AddsAmzDateHeader(t *testing.T) {
	cfg := BedrockConfig{
		Region: "us-east-1", AccessKeyID: "k", SecretAccessKey: "s",
	}
	req, _ := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/m/invoke",
		strings.NewReader(`{}`))
	_ = SignRequest(req, cfg)
	dt := req.Header.Get("X-Amz-Date")
	if dt == "" {
		dt = req.Header.Get("x-amz-date")
	}
	if dt == "" {
		t.Fatal("x-amz-date header missing")
	}
	if len(dt) != 16 || !strings.HasSuffix(dt, "Z") {
		t.Errorf("x-amz-date = %q, want ISO8601 basic format YYYYMMDDTHHMMSSZ", dt)
	}
}

func TestSignRequest_AddsSessionTokenWhenSet(t *testing.T) {
	cfg := BedrockConfig{
		Region: "us-east-1", AccessKeyID: "k", SecretAccessKey: "s",
		SessionToken: "fake-temp-token",
	}
	req, _ := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/m/invoke",
		strings.NewReader(`{}`))
	_ = SignRequest(req, cfg)
	if got := req.Header.Get("X-Amz-Security-Token"); got != "fake-temp-token" {
		t.Errorf("x-amz-security-token = %q, want fake-temp-token", got)
	}
}

// (proof 3, A′ scope) BY-VALUE: SignRequest takes BedrockConfig by VALUE — mutating the source struct
// after signing does not change the already-signed request (no pointer into mutable state). The
// configForProvider closure-snapshot semantics stay in proxy unchanged (the type/ConfigFor move is the
// deferred full-A step); A′ only moved SignRequest, which is a pure by-value function.
func TestSignRequest_ByValue_SourceMutationDoesNotAffectSignedRequest(t *testing.T) {
	cfg := BedrockConfig{Region: "us-east-1", AccessKeyID: "AKIA-ORIG", SecretAccessKey: "S1"}
	req, _ := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/m/invoke", strings.NewReader(`{}`))
	if err := SignRequest(req, cfg); err != nil {
		t.Fatal(err)
	}
	signedBefore := req.Header.Get("Authorization")

	// Mutate the SOURCE config to a different credential AFTER signing.
	cfg.AccessKeyID = "AKIA-CHANGED"
	cfg.SecretAccessKey = "S2"

	if got := req.Header.Get("Authorization"); got != signedBefore {
		t.Errorf("signed Authorization changed after source mutation: by-value broken")
	}
	if !strings.Contains(signedBefore, "Credential=AKIA-ORIG/") {
		t.Errorf("signed with the wrong (mutated) credential: %q", signedBefore)
	}
}
