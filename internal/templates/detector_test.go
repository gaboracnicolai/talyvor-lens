package templates

import (
	"context"
	"encoding/json"
	"testing"
)

func TestExtractSystemPrompt_OpenAIFormat(t *testing.T) {
	d := New(nil)
	body := []byte(`{"messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"hi"}]}`)

	got, ok := d.ExtractSystemPrompt(body)
	if !ok {
		t.Fatal("expected ok=true for OpenAI body with system message")
	}
	if got != "You are helpful" {
		t.Errorf("got %q, want %q", got, "You are helpful")
	}
}

func TestExtractSystemPrompt_AnthropicFormat(t *testing.T) {
	d := New(nil)
	body := []byte(`{"system":"You are an assistant","messages":[{"role":"user","content":"hi"}]}`)

	got, ok := d.ExtractSystemPrompt(body)
	if !ok {
		t.Fatal("expected ok=true for Anthropic body with system field")
	}
	if got != "You are an assistant" {
		t.Errorf("got %q, want %q", got, "You are an assistant")
	}
}

func TestExtractSystemPrompt_NotFound(t *testing.T) {
	d := New(nil)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	got, ok := d.ExtractSystemPrompt(body)
	if ok {
		t.Errorf("expected ok=false, got %q", got)
	}
}

func TestRecordAndPin_IncrementsHitCount(t *testing.T) {
	d := New(nil)
	ctx := context.Background()

	t1, pinned1 := d.RecordAndPin(ctx, "shared template", "openai")
	if t1.HitCount != 1 {
		t.Errorf("first call HitCount = %d, want 1", t1.HitCount)
	}
	if pinned1 {
		t.Error("first call should not be pinned")
	}

	t2, _ := d.RecordAndPin(ctx, "shared template", "openai")
	if t2.HitCount != 2 {
		t.Errorf("second call HitCount = %d, want 2", t2.HitCount)
	}
	// Should be the same in-memory template object.
	if t1.Hash != t2.Hash {
		t.Errorf("hashes differ for same content: %q vs %q", t1.Hash, t2.Hash)
	}
}

func TestRecordAndPin_PinnedAtHitCountTen(t *testing.T) {
	d := New(nil)
	ctx := context.Background()

	for i := 1; i <= 9; i++ {
		_, pinned := d.RecordAndPin(ctx, "popular template", "anthropic")
		if pinned {
			t.Fatalf("call %d returned pinned=true; should only pin at HitCount >= 10", i)
		}
	}
	tmpl, pinned := d.RecordAndPin(ctx, "popular template", "anthropic")
	if !pinned {
		t.Fatal("call 10 should return pinned=true")
	}
	if tmpl.HitCount != 10 {
		t.Errorf("HitCount = %d, want 10", tmpl.HitCount)
	}
	if tmpl.PinnedAt == nil {
		t.Error("PinnedAt should be set when pinned")
	}

	// Subsequent calls should NOT return pinned=true again.
	_, pinnedAgain := d.RecordAndPin(ctx, "popular template", "anthropic")
	if pinnedAgain {
		t.Error("subsequent calls after pinning should return pinned=false")
	}
}

func TestApplyAnthropicCaching_StringSystem(t *testing.T) {
	d := New(nil)
	body := []byte(`{"model":"claude","system":"You are helpful","messages":[{"role":"user","content":"hi"}]}`)

	out, err := d.ApplyAnthropicCaching(body, &Template{Content: "You are helpful"})
	if err != nil {
		t.Fatalf("ApplyAnthropicCaching: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sys, ok := got["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("system not converted to array; got %T value %v", got["system"], got["system"])
	}
	first, ok := sys[0].(map[string]any)
	if !ok {
		t.Fatalf("system[0] not an object; got %T", sys[0])
	}
	if first["type"] != "text" || first["text"] != "You are helpful" {
		t.Errorf("system[0] = %v, want type=text + text=You are helpful", first)
	}
	cc, ok := first["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" {
		t.Errorf("system[0].cache_control = %v, want {type:ephemeral}", first["cache_control"])
	}
}

func TestApplyAnthropicCaching_ArraySystem(t *testing.T) {
	d := New(nil)
	body := []byte(`{"model":"claude","system":[{"type":"text","text":"first"},{"type":"text","text":"second"}],"messages":[]}`)

	out, err := d.ApplyAnthropicCaching(body, &Template{Content: "second"})
	if err != nil {
		t.Fatalf("ApplyAnthropicCaching: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sys, ok := got["system"].([]any)
	if !ok || len(sys) != 2 {
		t.Fatalf("system not preserved as 2-element array; got %v", got["system"])
	}
	first := sys[0].(map[string]any)
	last := sys[1].(map[string]any)

	if _, has := first["cache_control"]; has {
		t.Errorf("first system element should not have cache_control; got %v", first)
	}
	cc, ok := last["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" {
		t.Errorf("last system element cache_control = %v, want {type:ephemeral}", last["cache_control"])
	}
}

func TestRecordAndPin_SameContentSameHash(t *testing.T) {
	d := New(nil)
	ctx := context.Background()

	t1, _ := d.RecordAndPin(ctx, "exact same template", "openai")
	t2, _ := d.RecordAndPin(ctx, "exact same template", "openai")
	if t1.Hash != t2.Hash {
		t.Errorf("same content should produce same hash; got %q vs %q", t1.Hash, t2.Hash)
	}

	t3, _ := d.RecordAndPin(ctx, "totally different template", "openai")
	if t3.Hash == t1.Hash {
		t.Errorf("different content should produce different hash; both got %q", t1.Hash)
	}
}
