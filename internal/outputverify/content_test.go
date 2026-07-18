package outputverify

import "testing"

// content_test.go — the CANONICAL CONTENT byte definition, pinned by vectors. CanonicalContent must return
// EXACTLY the bytes the flagship writer (talyvor-code agent, stripFences @ d6b8cc1) materializes on disk —
// byte-identical or the H5 content binding can never match a real tree. Every vector here is a normalization
// fork the definition resolves: fences, trailing newline, CRLF, multi-block, multi-choice, streaming bytes.

func TestCanonicalContent_AnthropicVectors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{
			// The common case: the model ends the code without a trailing newline → exactly one is appended
			// (the flagship writer appends it too — gofmt requires it).
			name: "bare code, no trailing newline",
			body: `{"content":[{"type":"text","text":"package main\nfunc main(){}"}]}`,
			want: "package main\nfunc main(){}\n",
			ok:   true,
		},
		{
			// Identity case: clean code ending in exactly one newline is UNCHANGED.
			name: "bare code, exactly one trailing newline",
			body: `{"content":[{"type":"text","text":"package main\nfunc main(){}\n"}]}`,
			want: "package main\nfunc main(){}\n",
			ok:   true,
		},
		{
			// Fenced: the opening fence line and the trailing fence are dropped.
			name: "fenced code block",
			body: "{\"content\":[{\"type\":\"text\",\"text\":\"```go\\npackage main\\nfunc main(){}\\n```\"}]}",
			want: "package main\nfunc main(){}\n",
			ok:   true,
		},
		{
			// Surrounding whitespace is trimmed before fence handling.
			name: "fenced with surrounding whitespace",
			body: "{\"content\":[{\"type\":\"text\",\"text\":\"\\n```go\\ncode()\\n```\\n\"}]}",
			want: "code()\n",
			ok:   true,
		},
		{
			// Prose after the closing fence: the trailing strip only fires when the text ENDS with the fence,
			// so the fence + prose survive. Quirky, but byte-equal to the flagship writer — the file on disk.
			name: "prose after closing fence (flagship quirk preserved)",
			body: "{\"content\":[{\"type\":\"text\",\"text\":\"```go\\ncode()\\n```\\nThanks!\"}]}",
			want: "code()\n```\nThanks!\n",
			ok:   true,
		},
		{
			// A blank line straight after the opening fence is preserved (no re-trim after the fence line).
			name: "blank line after opening fence preserved (flagship quirk)",
			body: "{\"content\":[{\"type\":\"text\",\"text\":\"```go\\n\\ncode()\\n```\"}]}",
			want: "\ncode()\n",
			ok:   true,
		},
		{
			// Interior CRLF is content — preserved byte-for-byte. Only OUTER whitespace is trimmed.
			name: "interior CRLF preserved",
			body: `{"content":[{"type":"text","text":"a\r\nb"}]}`,
			want: "a\r\nb\n",
			ok:   true,
		},
		{
			// Multi-block: every type=="text" block concatenated IN ORDER, no separator; non-text blocks
			// (tool_use / thinking) are skipped — exactly the flagship client's assembly.
			name: "multi text blocks concatenated, non-text skipped",
			body: `{"content":[{"type":"text","text":"A"},{"type":"tool_use","id":"x"},{"type":"text","text":"B"}]}`,
			want: "AB\n",
			ok:   true,
		},
		{name: "whitespace-only text → not committable", body: `{"content":[{"type":"text","text":"  \n "}]}`, ok: false},
		{name: "empty text → not committable", body: `{"content":[{"type":"text","text":""}]}`, ok: false},
		{name: "fence-only → not committable", body: "{\"content\":[{\"type\":\"text\",\"text\":\"```\"}]}", ok: false},
		{name: "no text blocks → not committable", body: `{"content":[{"type":"tool_use","id":"x"}]}`, ok: false},
		{name: "malformed JSON → not committable", body: `{"content":[`, ok: false},
		{name: "SSE stream bytes → not committable", body: "event: message_start\ndata: {}\n\n", ok: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := CanonicalContent("anthropic", []byte(c.body))
			if ok != c.ok {
				t.Fatalf("ok=%v, want %v (got %q)", ok, c.ok, got)
			}
			if ok && got != c.want {
				t.Errorf("canonical bytes = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCanonicalContent_OpenAIShapeVectors(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		body     string
		want     string
		ok       bool
	}{
		{
			name:     "openai choices[0].message.content",
			provider: "openai",
			body:     `{"choices":[{"message":{"content":"package main\nfunc main(){}"}}]}`,
			want:     "package main\nfunc main(){}\n",
			ok:       true,
		},
		{
			// Multi-choice: ONLY choices[0] binds — a second choice never leaks in.
			name:     "multi-choice takes index 0 only",
			provider: "openai",
			body:     `{"choices":[{"message":{"content":"first"}},{"message":{"content":"second"}}]}`,
			want:     "first\n",
			ok:       true,
		},
		{
			// Every non-anthropic provider is OpenAI-shaped at the capture site (native or reverse-translated)
			// — the same dispatch rule as inference.ExtractUsage.
			name:     "vllm is openai-shaped",
			provider: "vllm",
			body:     `{"choices":[{"message":{"content":"x = 1"}}]}`,
			want:     "x = 1\n",
			ok:       true,
		},
		{name: "empty content → not committable", provider: "openai", body: `{"choices":[{"message":{"content":""}}]}`, ok: false},
		{name: "no choices → not committable", provider: "openai", body: `{"choices":[]}`, ok: false},
		{name: "content not a string → not committable", provider: "openai", body: `{"choices":[{"message":{"content":[{"type":"text"}]}}]}`, ok: false},
		{name: "malformed JSON → not committable", provider: "openai", body: `not json`, ok: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := CanonicalContent(c.provider, []byte(c.body))
			if ok != c.ok {
				t.Fatalf("ok=%v, want %v (got %q)", ok, c.ok, got)
			}
			if ok && got != c.want {
				t.Errorf("canonical bytes = %q, want %q", got, c.want)
			}
		})
	}
}

// CanonicalContentSHA256 is definitionally Sha256Hex over the canonical bytes, same ok.
func TestCanonicalContentSHA256_MatchesCanonicalBytes(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"package main\nfunc main(){}"}]}`)
	content, ok := CanonicalContent("anthropic", body)
	if !ok {
		t.Fatal("vector must canonicalize")
	}
	sha, ok := CanonicalContentSHA256("anthropic", body)
	if !ok || sha != Sha256Hex([]byte(content)) {
		t.Fatalf("sha=%q ok=%v, want Sha256Hex(canonical)=%q", sha, ok, Sha256Hex([]byte(content)))
	}
	if _, ok := CanonicalContentSHA256("anthropic", []byte(`{"content":[]}`)); ok {
		t.Fatal("uncommittable body must yield ok=false")
	}
}
