// Package poolsafety checks that cross-tenant cache pooling cannot serve one workspace's
// response to an unrelated one because of shared prompt boilerplate.
//
// THE PROBLEM THIS EXISTS FOR — and it is NOT about any one client.
//
// A pooled semantic hit serves the contributing workspace's cached RESPONSE to a different
// tenant whenever the two prompt embeddings exceed LENS_SEMANTIC_THRESHOLD. That is only
// safe if two unrelated tenants' prompts land far apart in embedding space.
//
// Lens embeds every message concatenated (internal/proxy/proxy.go, extractPrompt). Real
// clients prepend a large FIXED system preamble to every request — an instruction block
// that is byte-identical across every user of that client, worldwide. talyvor-code ships a
// ~720-character one; LangChain, Cursor, Continue and most agent frameworks are comparable
// or considerably larger. That shared text is a substantial share of what gets embedded,
// and it pulls unrelated requests together. Two companies asking about entirely different
// proprietary code, through the same client, can look far more alike than their code does.
//
// WHETHER THAT REACHES THE THRESHOLD DEPENDS ON THE EMBEDDING MODEL. Measured on two
// unrelated codebases (a Go payments ledger, a Python imaging pipeline) wrapped in the same
// shipped review preamble:
//
//	text-embedding-3-small (Lens's default) ..... 0.69   safe, 0.23 below threshold
//	all-MiniLM-L6-v2 ........................... 0.84   uncomfortable
//	BAAI/bge-small-en-v1.5 ..................... 0.985  WOULD SERVE ACROSS TENANTS
//
// Same code, same prompts, same threshold — three different safety outcomes depending on
// what LENS_EMBEDDING_MODEL names. Today's value is safe. Nothing enforced that, and
// nothing would have noticed a change: swapping the embedding model for cost or latency is
// an ordinary operational decision that nobody would route past a security review.
//
// So this package does not change caching behaviour. It makes the dependency CHECKED:
// embed a corpus of "same preamble, unrelated content" pairs through the CONFIGURED
// embedder and refuse to call the configuration safe if any pair reaches the threshold.
// Run it at deploy preflight, and after any change to the embedding model, the threshold,
// or a major client's prompt templates — each of which silently moves this number.
package poolsafety

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Pair is one "same preamble, unrelated content" case. A and B are payloads from
// deliberately unrelated domains; Preamble is the fixed instruction block a real client
// prepends to both.
type Pair struct {
	Name     string
	Preamble string
	A, B     string
	Note     string
}

// Full renders the prompt as a client would send it and as extractPrompt would concatenate
// it: the fixed preamble followed by the variable payload.
func (p Pair) Full(payload string) string { return p.Preamble + "\n\n" + payload }

// Result is one evaluated pair.
type Result struct {
	Pair       Pair
	Similarity float64
	Violates   bool
}

// Report is the outcome across the whole corpus.
type Report struct {
	Threshold float64
	Pairs     []Result
	Worst     Result
	OK        bool
}

func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pool-safety check (threshold %.2f): ", r.Threshold)
	if r.OK {
		fmt.Fprintf(&b, "OK — worst pair %.4f (%s)\n", r.Worst.Similarity, r.Worst.Pair.Name)
	} else {
		fmt.Fprintf(&b, "UNSAFE\n")
	}
	for _, p := range r.Pairs {
		flag := "    "
		if p.Violates {
			flag = " !! "
		}
		fmt.Fprintf(&b, "%s%-46s %.4f  %s\n", flag, p.Pair.Name, p.Similarity, p.Pair.Note)
	}
	if !r.OK {
		fmt.Fprintf(&b, "\nAt least one pair of UNRELATED prompts reached the pooling threshold.\n"+
			"With cross-tenant pooling enabled, a response generated for one workspace can be\n"+
			"served to another. Either raise LENS_SEMANTIC_THRESHOLD above the worst score,\n"+
			"choose an embedding model with a wider dynamic range, or disable pooling\n"+
			"(LENS_CACHE_POOLABLE_ENABLED=false) until it is resolved.\n")
	}
	return b.String()
}

// Two unrelated domains. The point is that nothing about the payloads is similar: different
// languages, different industries, no shared identifiers. Any similarity the check measures
// comes from the shared preamble, which is exactly what is being tested.
const (
	goLedgerDiff = `--- a/ledger/post.go
+++ b/ledger/post.go
@@ -40,7 +40,9 @@ func (l *Ledger) postEntry(ctx context.Context, e Entry) error {
 	acct := l.accounts[e.AccountID]
-	if acct.Balance < e.DebitMinor {
+	if acct.Balance < e.DebitMinor && !e.AllowOverdraft {
 		return ErrInsufficientFunds
 	}
 	acct.Balance -= e.DebitMinor
+	l.metrics.PostedMinor.Add(float64(e.DebitMinor))
 	return l.journal.Append(ctx, e)`

	pyImagingDiff = `--- a/imaging/resample.py
+++ b/imaging/resample.py
@@ -40,7 +40,9 @@ def resample_series(study, spacing_mm):
     vol = study.pixel_array
-    if vol.ndim != 3:
+    if vol.ndim != 3 or vol.size == 0:
         raise ValueError("expected a volumetric series")
     factor = study.spacing / spacing_mm
+    logger.debug("resampling %s by %s", study.uid, factor)
     return zoom(vol, factor, order=1)`

	goLedgerSnip = `func (l *Ledger) postEntry(ctx context.Context, e Entry) error {
	acct := l.accounts[e.AccountID]
	if acct.Balance < e.DebitMinor { return ErrInsufficientFunds }
	return l.journal.Append(ctx, e)
}`

	pyImagingSnip = `def resample_series(study, spacing_mm):
    vol = study.pixel_array
    if vol.ndim != 3: raise ValueError("expected a volumetric series")
    return zoom(vol, factor, order=1)`
)

// Preambles are representative of what real clients send. The review block is quoted from
// talyvor-code because it is the largest one available to measure, not because this is a
// talyvor-code concern — any client with a preamble of this size has the same profile, and
// several ship considerably more.
const (
	reviewPreamble = `You are an expert code reviewer performing a pull-request review. Analyze the diff carefully and focus on: Bugs and logic errors, security vulnerabilities, performance issues, code quality, and maintainability.

Structure your review as Markdown with these sections:

## PR Summary
<2-3 sentence summary of what this PR does>

## Review

### Critical Issues
<blocking issues that must be fixed>

### Warnings
<non-blocking issues worth addressing>

### Suggestions
<optional improvements>

## Verdict
APPROVE / REQUEST CHANGES / NEEDS DISCUSSION`

	assistantPreamble = `You are an expert coding assistant. When showing code, use markdown code fences with the language identifier. Be concise but complete.`

	// A deliberately oversized block, standing in for the agent frameworks whose system
	// prompts run to several thousand characters. If a configuration survives this, the
	// smaller real-world preambles are covered by implication.
	agentPreamble = `You are an autonomous software engineering agent operating inside a user's repository.
You have access to tools for reading files, writing files, searching the codebase, and running shell commands.
Always read a file before editing it. Never write outside the workspace root. Prefer minimal, surgical diffs.
When a task is ambiguous, state your assumption and continue rather than stopping to ask.
Explain your reasoning briefly before each tool call, and summarise what changed at the end.
Do not fabricate file contents, test results, or command output. If a command fails, report the failure verbatim.
Follow the project's existing conventions for naming, formatting, error handling, and testing.
Write tests for new behaviour. Run the test suite before declaring a task complete.`
)

// Corpus returns the pairs the check evaluates. Exported so the corpus itself can be
// asserted on — a check whose corpus drifted into identical or preamble-less pairs would
// pass while measuring nothing.
func Corpus() []Pair {
	return []Pair{
		{
			Name:     "review preamble · unrelated diffs",
			Preamble: reviewPreamble,
			A:        goLedgerDiff,
			B:        pyImagingDiff,
			Note:     "realistic PR review from two companies",
		},
		{
			Name:     "review preamble · tiny diffs",
			Preamble: reviewPreamble,
			A:        "-\tif acct.Balance < e.DebitMinor {\n+\tif acct.Balance < e.DebitMinor && !e.AllowOverdraft {",
			B:        "-    if vol.ndim != 3:\n+    if vol.ndim != 3 or vol.size == 0:",
			Note:     "one-line PRs — the preamble's largest share",
		},
		{
			Name:     "assistant preamble · unrelated snippets",
			Preamble: assistantPreamble,
			A:        "why is this nil\n" + goLedgerSnip,
			B:        "why is this nil\n" + pyImagingSnip,
			Note:     "same generic question, different code",
		},
		{
			Name:     "large agent preamble · unrelated snippets",
			Preamble: agentPreamble,
			A:        "why is this nil\n" + goLedgerSnip,
			B:        "why is this nil\n" + pyImagingSnip,
			Note:     "stands in for framework-sized system prompts",
		},
	}
}

// Embedder mirrors cache.Embedder so this package can be handed the very embedder the
// serve path uses, rather than a copy that might diverge from it.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Check embeds every corpus pair through emb and reports whether any UNRELATED pair reaches
// threshold. An embedder error is returned, never absorbed: a check that reported "safe"
// because it could not measure would be worse than no check.
func Check(ctx context.Context, emb Embedder, threshold float64) (Report, error) {
	if emb == nil {
		return Report{}, errors.New("poolsafety: nil embedder")
	}
	if threshold <= 0 || threshold > 1 {
		return Report{}, fmt.Errorf("poolsafety: implausible threshold %v (want 0 < t <= 1)", threshold)
	}

	rep := Report{Threshold: threshold, OK: true}
	for _, p := range Corpus() {
		va, err := emb.Embed(ctx, p.Full(p.A))
		if err != nil {
			return Report{}, fmt.Errorf("poolsafety: embed %q A: %w", p.Name, err)
		}
		vb, err := emb.Embed(ctx, p.Full(p.B))
		if err != nil {
			return Report{}, fmt.Errorf("poolsafety: embed %q B: %w", p.Name, err)
		}
		sim, err := cosine(va, vb)
		if err != nil {
			return Report{}, fmt.Errorf("poolsafety: %q: %w", p.Name, err)
		}
		res := Result{Pair: p, Similarity: sim, Violates: sim >= threshold}
		rep.Pairs = append(rep.Pairs, res)
		if sim > rep.Worst.Similarity {
			rep.Worst = res
		}
		if res.Violates {
			rep.OK = false
		}
	}
	return rep, nil
}

func cosine(a, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("dimension mismatch: %d vs %d", len(a), len(b))
	}
	if len(a) == 0 {
		return 0, errors.New("empty embedding")
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0, errors.New("zero-magnitude embedding")
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb)), nil
}
