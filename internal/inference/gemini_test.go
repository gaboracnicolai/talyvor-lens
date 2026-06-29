package inference

import (
	"encoding/json"
	"testing"
)

// Relocated from internal/proxy/gemini_test.go (PR-3b A′) — the translate UNIT tests move with their
// funcs. Byte-identical except the package line + the now-exported call names (translateToGemini →
// TranslateToGemini, translateFromGemini → TranslateFromGemini). The handler-level TestHandleGoogle_*
// tests stay in package proxy, byte-identical.

func TestTranslateToGemini_ConvertsMessagesCorrectly(t *testing.T) {
	body := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hello"}]}`)

	out, model, err := TranslateToGemini(body)
	if err != nil {
		t.Fatalf("translateToGemini: %v", err)
	}
	if model != "gemini-2.5-pro" {
		t.Errorf("model = %q, want gemini-2.5-pro", model)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	contents, ok := got["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("contents shape wrong: %v", got["contents"])
	}
	c0 := contents[0].(map[string]any)
	if c0["role"] != "user" {
		t.Errorf("role = %v, want user", c0["role"])
	}
	parts, _ := c0["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("parts wrong: %v", parts)
	}
	if p0 := parts[0].(map[string]any); p0["text"] != "hello" {
		t.Errorf("text = %v, want hello", p0["text"])
	}
}

func TestTranslateToGemini_ExtractsSystemMessage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"hi"}]}`)

	out, _, err := TranslateToGemini(body)
	if err != nil {
		t.Fatalf("translateToGemini: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)

	sys, ok := got["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction missing: %v", got)
	}
	sysParts := sys["parts"].([]any)
	if len(sysParts) != 1 || sysParts[0].(map[string]any)["text"] != "You are helpful" {
		t.Errorf("systemInstruction parts = %v", sysParts)
	}

	// Only the user message should remain in contents.
	contents := got["contents"].([]any)
	if len(contents) != 1 {
		t.Errorf("contents = %d entries, want 1 (system should not appear in contents)", len(contents))
	}
}

func TestTranslateToGemini_MapsAssistantToModel(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello back"}]}`)

	out, _, err := TranslateToGemini(body)
	if err != nil {
		t.Fatalf("translateToGemini: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)

	contents := got["contents"].([]any)
	if len(contents) != 2 {
		t.Fatalf("contents = %d entries, want 2", len(contents))
	}
	if contents[0].(map[string]any)["role"] != "user" {
		t.Errorf("contents[0].role = %v, want user", contents[0].(map[string]any)["role"])
	}
	if contents[1].(map[string]any)["role"] != "model" {
		t.Errorf("contents[1].role = %v, want model (assistant should map to model)", contents[1].(map[string]any)["role"])
	}
}

func TestTranslateFromGemini_ConvertsResponseCorrectly(t *testing.T) {
	gemini := []byte(`{"candidates":[{"content":{"parts":[{"text":"hi from gemini"}],"role":"model"}}]}`)

	out, err := TranslateFromGemini(gemini, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("translateFromGemini: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", got["object"])
	}
	if got["model"] != "gemini-2.5-flash" {
		t.Errorf("model = %v, want gemini-2.5-flash", got["model"])
	}
	choices := got["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(choices))
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["role"] != "assistant" {
		t.Errorf("message.role = %v, want assistant", msg["role"])
	}
	if msg["content"] != "hi from gemini" {
		t.Errorf("message.content = %v, want %q", msg["content"], "hi from gemini")
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("finish_reason missing or wrong: %v", choices[0])
	}
}
