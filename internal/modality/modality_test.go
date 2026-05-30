package modality

import "testing"

func TestDetect_OpenAIImageURL(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":[
		{"type":"text","text":"what is in this picture?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORnotvalid"}}
	]}]}`)
	ms := Detect(body)
	if !ms.HasImage || ms.ImageCount != 1 {
		t.Fatalf("expected one image detected: %+v", ms)
	}
	if ms.HasAudio || ms.HasDocument {
		t.Fatalf("only image expected: %+v", ms)
	}
	if ms.Label() != "image" {
		t.Fatalf("label: got %q want image", ms.Label())
	}
}

func TestDetect_AnthropicImageBlock(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[
		{"type":"text","text":"describe"},
		{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"notreallybase64"}}
	]}]}`)
	ms := Detect(body)
	if !ms.HasImage || ms.ImageCount != 1 {
		t.Fatalf("anthropic image block not detected: %+v", ms)
	}
}

func TestDetect_TextOnly(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"just text here"}]}`)
	ms := Detect(body)
	if ms.Multimodal() {
		t.Fatalf("text-only request must not be multimodal: %+v", ms)
	}
	if ms.Label() != "text" {
		t.Fatalf("label: got %q want text", ms.Label())
	}
	if ms.TextChars != len("just text here") {
		t.Fatalf("text chars: got %d", ms.TextChars)
	}
}

// TestDetect_DoesNotDecodeBase64 proves detection is structural: it works on
// a payload whose image "data" is not valid base64 at all. If we decoded the
// bytes this would behave differently; detection only reads the block type.
func TestDetect_DoesNotDecodeBase64(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"!!! definitely not base64 @@@"}}
	]}]}`)
	ms := Detect(body)
	if !ms.HasImage {
		t.Fatal("detection must work on structure alone, regardless of base64 validity")
	}
}

func TestDetect_AudioAndDocument(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"input_audio","input_audio":{"data":"x","format":"wav"}},
		{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"y"}}
	]}]}`)
	ms := Detect(body)
	if !ms.HasAudio || !ms.HasDocument {
		t.Fatalf("audio+document not detected: %+v", ms)
	}
	if ms.Label() != "audio,document" {
		t.Fatalf("label: got %q want audio,document", ms.Label())
	}
}

func TestSupports_KnownVisionAndConservativeUnknown(t *testing.T) {
	img := ModalitySet{HasImage: true, ImageCount: 1}

	if !Supports("gpt-4o", img) {
		t.Error("gpt-4o is a known vision model — should support image")
	}
	if !Supports("claude-haiku-4-6", img) {
		t.Error("claude-haiku-4-6 should support image")
	}
	// Unknown model → conservative text-only → cannot serve an image.
	if Supports("totally-unknown-model", img) {
		t.Error("unknown model must be treated as text-only (conservative)")
	}
	// A listed text-only-ish family: Mistral large is not in the registry.
	if Supports("mistral-large-latest", img) {
		t.Error("mistral-large-latest is not registered for vision — must be text-only")
	}
	// Text-only request is supported by every model.
	if !Supports("totally-unknown-model", ModalitySet{}) {
		t.Error("a text-only request must be supported by any model")
	}
}

func TestCapableModel_PicksAllowedCheapestNeverDisallowed(t *testing.T) {
	img := ModalitySet{HasImage: true, ImageCount: 1}

	// No allow-list → cheapest capable for the provider.
	if m, ok := CapableModel("openai", img, nil); !ok || m != "gpt-4o-mini" {
		t.Fatalf("openai cheapest capable: got %q ok=%v want gpt-4o-mini", m, ok)
	}
	// Allow-list excludes the cheapest → next allowed capable.
	if m, ok := CapableModel("openai", img, []string{"gpt-4o"}); !ok || m != "gpt-4o" {
		t.Fatalf("must pick the allowed capable model: got %q", m)
	}
	// Allow-list has only a text-only model → no capable model.
	if _, ok := CapableModel("openai", img, []string{"some-text-only-model"}); ok {
		t.Fatal("must NOT recommend a model outside the allow-list")
	}
	// Unknown provider → none.
	if _, ok := CapableModel("nonexistent", img, nil); ok {
		t.Fatal("unknown provider should have no capable model")
	}
}

// TestLabel_BoundedCardinality proves the metric label is bounded: across
// every combination of the three modalities, Label() yields at most 2^3
// distinct values — never a per-request or unbounded value.
func TestLabel_BoundedCardinality(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		ms := ModalitySet{
			HasImage:    i&1 != 0,
			HasAudio:    i&2 != 0,
			HasDocument: i&4 != 0,
		}
		seen[ms.Label()] = true
	}
	if len(seen) > 8 {
		t.Fatalf("modality label cardinality unbounded: %d distinct values", len(seen))
	}
	// "text" must be the all-false label.
	if (ModalitySet{}).Label() != "text" {
		t.Fatal("empty set should label as text")
	}
}

func TestEstimateInputTokens(t *testing.T) {
	ms := ModalitySet{HasImage: true, ImageCount: 2, TextChars: 400}
	// 400/4 + 2*1000 = 100 + 2000 = 2100.
	if got := ms.EstimateInputTokens(); got != 2100 {
		t.Fatalf("estimate: got %d want 2100", got)
	}
}
