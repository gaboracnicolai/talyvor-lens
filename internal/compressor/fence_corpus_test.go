package compressor

// THE FENCED CORPUS — the population one of the four advertised techniques was
// measured by NOTHING against.
//
// reach_test.go measures 308 prose prompts and 8 agent-traffic prompts and reports
// "0 modified" and "8 of 8 rewritten". fence_reach_test.go measured why both
// numbers describe the rewriter ON PROSE: 0 of those 316 prompts contain a ```
// fence at all, so `code_blocks` — the technique whose own machinery was caught
// relocating a customer's code block (placeholder_collision_test.go) — ran on
// nothing any reach test could see.
//
// This file is that missing population. Every prompt below is ORDINARY: a review
// request, a README, a truncated log paste, a numbered list of steps. None is
// adversarial and none contains a NUL. They are what a coding agent and a human
// actually post at a gateway.
//
// ⚠ WHAT IT MEASURES IS NOT WHAT THE FENCE WAS ADVERTISED TO DO. See
// fence_pairing_test.go: the "``` fence is the boundary" claim holds only while
// every ``` in the prompt is either an opener or the closer of the block before
// it. It is the PAIRING that is the boundary, not the fence.

import "strings"

// fencedPrompt is one corpus entry with its outcome PINNED. The pins are
// per-prompt rather than aggregate because a total cannot see one case going quiet
// while another starts firing.
//
// ⚠ wantOut IS THE FULL OUTPUT BYTES, AND IT IS THAT BECAUSE A WEAKER PIN WAS
// CAUGHT BEING BLIND. This struct first carried `wantModified bool` beside
// wantTechniques. C27 of w61-fencecorpus-controls-9d72.py line-anchors the fence
// regex — the smallest real fix for the stray-``` inversion, which turns a
// destroyed python block back into an intact one — and THIS TEST DID NOT NOTICE.
// The prompt was still "modified" (the trailing newline is still trimmed) and the
// technique list was still ["whitespace"], so both pins held while the bytes the
// provider would receive changed completely. A boolean that says "something
// changed" is the same defect as SavingsPct reading 0.00% on a modified prompt:
// it answers a question nobody is asking.
type fencedPrompt struct {
	name string
	in   string
	// wantOut is the exact CompressedPrompt. Equal to `in` where the rewriter
	// leaves the prompt alone.
	wantOut string
	// wantTechniques is the exact TechniquesApplied slice, in order. Pinned
	// separately from the bytes because attribution is the product's stated pitch
	// and can move on its own — C34 credits code_blocks everywhere without
	// changing a byte.
	wantTechniques []string
	// literals lists, VERBATIM, the source spellings of string literals in this
	// prompt whose value a line-oriented rewrite could alter — a Python
	// triple-quoted block, a Go backtick raw string, or a one-line literal whose
	// trailing run of spaces is inside the quotes. Each is asserted to appear in
	// `in` before anything is concluded from it (TestFenceCorpus_LiteralCensus),
	// because a literal that is not in the prompt SURVIVES VACUOUSLY: a typo in
	// this field would otherwise read as evidence of losslessness.
	//
	// Empty for the prompts that carry no such literal, which is most of them.
	// The two lossless entries (JSON, SQL) carry none BY DESIGN — that is the
	// difference this field exists to measure.
	literals []string
	// why names what the outcome means for the caller. A corruption says so.
	why string
}

// fencedCorpus is the committed population. Its size is pinned by
// TestFenceCorpus_TheFencedPopulationIsMeasured.
func fencedCorpus() []fencedPrompt {
	return []fencedPrompt{
		// ---- the technique behaving as advertised -------------------------------
		{
			name:           "go review, clean block",
			in:             "Review this:\n```go\nfunc a() {\n\tif x {\n\t\treturn 1\n\t}\n}\n```\nthanks",
			wantOut:        "Review this:\n```go\nfunc a() {\n\tif x {\n\t\treturn 1\n\t}\n}\n```\nthanks",
			wantTechniques: nil,
			why:            "tab indentation inside a correctly paired fence survives — this is the advertised behaviour",
		},
		{
			name:           "unified diff in a fence",
			in:             "```diff\n--- a/ledger/post.go\n+++ b/ledger/post.go\n@@\n-\tif acct.Balance < e.DebitMinor {\n+\tif acct.Balance < e.DebitMinor && !e.AllowOverdraft {\n```",
			wantOut:        "```diff\n--- a/ledger/post.go\n+++ b/ledger/post.go\n@@\n-\tif acct.Balance < e.DebitMinor {\n+\tif acct.Balance < e.DebitMinor && !e.AllowOverdraft {\n```",
			wantTechniques: nil,
			why:            "a diff's leading tabs and column alignment survive inside a paired fence",
		},
		{
			name:           "python, clean block",
			in:             "Fix this:\n```python\ndef f(vol):\n    if vol.ndim != 3:\n        raise ValueError('rank')\n    return vol.sum()\n```",
			wantOut:        "Fix this:\n```python\ndef f(vol):\n    if vol.ndim != 3:\n        raise ValueError('rank')\n    return vol.sum()\n```",
			wantTechniques: nil,
			why:            "python nesting survives inside a paired fence — the case TestReach_UnfencedPythonNestingCollapsesButFencedSurvives pins",
		},
		{
			name:           "prose rewritten, block untouched",
			in:             "Could you please explain, in order to help me review it:\n```go\nx := 1\n```\n",
			wantOut:        "explain, to help me review it:\n```go\nx := 1\n```",
			wantTechniques: []string{"whitespace", "redundant_phrases", "common_patterns"},
			why:            "the prose techniques fire and the fenced block is left alone — the intended split of labour",
		},

		// ---- lossless inside the fence: whitespace-insignificant languages ------
		{
			name:           "json body with a blank line",
			in:             "```json\n{\n  \"a\": 1,\n\n  \"b\": 2\n}\n```",
			wantOut:        "```json\n{\n  \"a\": 1,\n  \"b\": 2\n}\n```",
			wantTechniques: []string{"code_blocks"},
			why:            "JSON is whitespace-insignificant outside strings, so dropping the blank line is a REAL and lossless saving",
		},
		{
			name:           "sql body with a blank line",
			in:             "```sql\nSELECT a\nFROM t\n\nWHERE a > 1;\n```",
			wantOut:        "```sql\nSELECT a\nFROM t\nWHERE a > 1;\n```",
			wantTechniques: []string{"code_blocks"},
			why:            "same: SQL does not carry meaning in blank lines",
		},

		// ---- lossy inside the fence: the value of a multi-line literal ----------
		{
			name:           "python triple-quoted literal loses a blank line",
			in:             "explain:\n```python\ns = \"\"\"a\n\nb\"\"\"\nprint(s)\n```",
			wantOut:        "explain:\n```python\ns = \"\"\"a\nb\"\"\"\nprint(s)\n```",
			wantTechniques: []string{"code_blocks"},
			literals:       []string{"\"\"\"a\n\nb\"\"\""},
			why:            "CORRUPTION: the literal's value changes from \"a\\n\\nb\" to \"a\\nb\" — a different program",
		},
		{
			name:           "python triple-quoted literal loses trailing spaces",
			in:             "explain:\n```python\ns = \"\"\"a   \nb\"\"\"\nprint(s)\n```",
			wantOut:        "explain:\n```python\ns = \"\"\"a\nb\"\"\"\nprint(s)\n```",
			wantTechniques: []string{"code_blocks"},
			literals:       []string{"\"\"\"a   \nb\"\"\""},
			why:            "CORRUPTION: right-trim removes bytes that are part of the literal's value",
		},
		{
			name:           "go raw string literal loses a blank line",
			in:             "```go\nconst s = `line1\n\nline2`\n```",
			wantOut:        "```go\nconst s = `line1\nline2`\n```",
			wantTechniques: []string{"code_blocks"},
			literals:       []string{"`line1\n\nline2`"},
			why:            "CORRUPTION: Go backtick literals are multi-line too; the same deletion applies",
		},
		{
			name:           "markdown fixture inside a go fence loses trailing spaces",
			in:             "```go\nwant := \"line one   \\nline two\"\nx := 1   \n```",
			wantOut:        "```go\nwant := \"line one   \\nline two\"\nx := 1\n```",
			wantTechniques: []string{"code_blocks"},
			literals:       []string{"\"line one   \\nline two\""},
			why:            "the trailing-space run on a real code line is a fair saving; the one inside the literal is escaped and survives",
		},

		// ---- the fence is not the boundary: PAIRING is ---------------------------
		{
			name:           "a stray ``` in prose inverts the block that follows",
			in:             "Wrap it in ``` fences, like:\n```python\ndef f():\n    if x:\n        return 1\n```\n",
			wantOut:        "Wrap it in ``` fences, like:\n```python\ndef f():\n if x:\n return 1\n```",
			wantTechniques: []string{"whitespace"},
			why:            "CORRUPTION: the prose pairs with the OPENING fence, so the python body is treated as prose and its nesting collapses",
		},
		{
			name:           "a stray ``` inverts EVERY block after it",
			in:             "Use ``` to fence.\n```go\nfunc a() {\n\tx := 1\n}\n```\nand\n```go\nfunc b() {\n\ty := 2\n}\n```\n",
			wantOut:        "Use ``` to fence.\n```go\nfunc a() {\n x := 1\n}\n```\nand\n```go\nfunc b() {\n y := 2\n}\n```",
			wantTechniques: []string{"whitespace"},
			why:            "CORRUPTION: the shift is not local — both Go bodies lose their tabs and both prose gaps are 'protected'",
		},
		{
			name:           "nested fences: a README that contains code",
			in:             "Fix my README:\n```markdown\n# Title\n\n```python\ndef f():\n    if x:\n        return 1\n```\n\nDone.\n```\n",
			wantOut:        "Fix my README:\n```markdown\n# Title\n```python\ndef f():\n if x:\n return 1\n```\nDone.\n```",
			wantTechniques: []string{"whitespace", "code_blocks"},
			why:            "CORRUPTION: the inner python is outside every matched pair, so it collapses inside a fence the caller wrote",
		},
		{
			name:           "unterminated fence: a truncated log paste",
			in:             "Here is the log:\n```\n2026-01-01  ERROR   boom\n2026-01-01  INFO    ok\n",
			wantOut:        "Here is the log:\n```\n2026-01-01 ERROR boom\n2026-01-01 INFO ok",
			wantTechniques: []string{"whitespace"},
			why:            "CORRUPTION: an opened-but-unclosed fence protects nothing; the log's column alignment is destroyed",
		},

		// ---- markdown constructs this rewriter does not treat as code at all -----
		{
			name:           "tilde fence",
			in:             "Review:\n~~~python\ndef f():\n    if x:\n        return 1\n~~~\n",
			wantOut:        "Review:\n~~~python\ndef f():\n if x:\n return 1\n~~~",
			wantTechniques: []string{"whitespace"},
			why:            "CORRUPTION: ~~~ is an equally valid CommonMark fence and this rewriter does not know it exists",
		},
		{
			name:           "four-space indented code block",
			in:             "Review:\n\n    def f():\n        if x:\n            return 1\n\nthanks",
			wantOut:        "Review:\n\n def f():\n if x:\n return 1\n\nthanks",
			wantTechniques: []string{"whitespace"},
			why:            "CORRUPTION: the indented code block is the oldest markdown code form and it is invisible here",
		},
		{
			name:           "fence indented inside a numbered list item",
			in:             "Steps:\n\n1. run this:\n\n   ```sh\n   make  test\n   ```\n\n2. done\n",
			wantOut:        "Steps:\n\n1. run this:\n\n ```sh\n   make  test\n   ```\n\n2. done",
			wantTechniques: []string{"whitespace"},
			why:            "the opening fence's own indentation is outside the match and is collapsed while the closing fence keeps its three spaces — the block stops being a list child",
		},
	}
}

// fencedCorpusPrompts renders the corpus as a flat prompt list, for the census in
// fence_reach_test.go. Sharing this one renderer is deliberate: a census assembled
// from its own copy of the corpus would not be a census of what is measured.
func fencedCorpusPrompts() []string {
	c := fencedCorpus()
	out := make([]string, 0, len(c))
	for _, p := range c {
		out = append(out, p.in)
	}
	return out
}

// fencedCorpusCarryingAFence counts corpus entries that actually contain a ```
// fence. Two entries deliberately do not (the tilde fence and the indented block)
// — they are in this corpus BECAUSE markdown calls them code and this rewriter
// does not, and the census must not quietly claim them as fenced coverage.
func fencedCorpusCarryingAFence() int {
	n := 0
	for _, p := range fencedCorpus() {
		if strings.Contains(p.in, "```") {
			n++
		}
	}
	return n
}
