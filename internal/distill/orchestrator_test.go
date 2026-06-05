package distill

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeConv is an in-memory IsolatedConverter — stands in for *ProcessIsolator
// so the orchestrator tests don't spawn a subprocess.
type fakeConv struct {
	calls int
	res   Result
	err   error
}

func (f *fakeConv) Convert(_ context.Context, _ []byte, format Format) (Result, error) {
	f.calls++
	r := f.res
	r.Format = format
	return r, f.err
}

// Orchestrate ties isolated Convert → ApplyTier → cache → ComputeSavings. With
// no cache it converts every call and reports honest savings.
func TestOrchestrate_NoCache_ConvertsAndMeasures(t *testing.T) {
	in := []byte("<html><body><h1>Hi</h1><p>there</p></body></html>")
	conv := &fakeConv{res: Result{Markdown: "# Hi\n\nthere"}}

	res, sav, err := Orchestrate(context.Background(), conv, nil, in, FormatHTML, TierFaithful)
	if err != nil {
		t.Fatal(err)
	}
	if conv.calls != 1 {
		t.Fatalf("converter should run once; calls=%d", conv.calls)
	}
	if res.Markdown != "# Hi\n\nthere" || res.Format != FormatHTML {
		t.Errorf("result wrong: %q / %q", res.Markdown, res.Format)
	}
	if res.Tier != TierFaithful {
		t.Errorf("tier not recorded: %q", res.Tier)
	}
	if sav.InputTokensRaw != len(in)/4 || sav.InputTokensDistilled != len(res.Markdown)/4 {
		t.Errorf("savings basis wrong: raw=%d distilled=%d", sav.InputTokensRaw, sav.InputTokensDistilled)
	}
}

// A tier is applied parent-side on the isolated (faithful) output.
func TestOrchestrate_AppliesTier(t *testing.T) {
	conv := &fakeConv{res: Result{Markdown: "# Title\n\nbody to drop\n\n## Sec\n\nmore"}}
	res, _, err := Orchestrate(context.Background(), conv, nil, []byte("x"), FormatHTML, TierOutline)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tier != TierOutline {
		t.Errorf("tier=%q want outline", res.Tier)
	}
	if strings.Contains(res.Markdown, "body to drop") {
		t.Errorf("outline must drop body: %q", res.Markdown)
	}
}

// NeedsVision passes through untouched (live vision is a later stage) and is
// NOT cached (no usable Markdown).
func TestOrchestrate_NeedsVisionPassthrough(t *testing.T) {
	c := &fakeCache{}
	conv := &fakeConv{res: Result{NeedsVision: true, Markdown: ""}}
	res, sav, err := Orchestrate(context.Background(), conv, c, []byte("%PDF-scan"), FormatPDF, TierFaithful)
	if err != nil {
		t.Fatal(err)
	}
	if !res.NeedsVision || res.Markdown != "" {
		t.Errorf("NeedsVision must pass through; needsVision=%v md=%q", res.NeedsVision, res.Markdown)
	}
	if sav.TokensSaved != 0 {
		t.Errorf("NeedsVision saves nothing; got %d", sav.TokensSaved)
	}
	if c.sets != 0 {
		t.Errorf("a NeedsVision result must NOT be cached; sets=%d", c.sets)
	}
}

// Cache miss converts + stores; a second call HITS and skips conversion.
func TestOrchestrate_CacheMissThenHit(t *testing.T) {
	c := &fakeCache{}
	in := []byte("<p>hello cached</p>")
	conv := &fakeConv{res: Result{Markdown: "hello cached"}}

	r1, s1, err := Orchestrate(context.Background(), conv, c, in, FormatHTML, TierFaithful)
	if err != nil {
		t.Fatal(err)
	}
	if conv.calls != 1 || c.sets != 1 || s1.CacheHit {
		t.Fatalf("miss path: calls=%d sets=%d hit=%v", conv.calls, c.sets, s1.CacheHit)
	}
	r2, s2, err := Orchestrate(context.Background(), conv, c, in, FormatHTML, TierFaithful)
	if err != nil {
		t.Fatal(err)
	}
	if conv.calls != 1 {
		t.Errorf("hit must NOT re-convert; calls=%d", conv.calls)
	}
	if !s2.CacheHit || r2.Markdown != r1.Markdown {
		t.Errorf("hit must serve the cached result; hit=%v md=%q", s2.CacheHit, r2.Markdown)
	}
}

// A conversion error surfaces (the caller decides to pass the original request
// through); nothing is cached.
func TestOrchestrate_ConvertError(t *testing.T) {
	c := &fakeCache{}
	conv := &fakeConv{err: errors.New("worker exploded")}
	_, _, err := Orchestrate(context.Background(), conv, c, []byte("x"), FormatPDF, TierFaithful)
	if err == nil {
		t.Fatal("expected the conversion error to surface")
	}
	if c.sets != 0 {
		t.Errorf("a failed conversion must not cache; sets=%d", c.sets)
	}
}

// FormatFromMediaType maps the chat-block media types to distill formats.
func TestFormatFromMediaType(t *testing.T) {
	cases := map[string]Format{
		"application/pdf":  FormatPDF,
		"text/html":        FormatHTML,
		"text/csv":         FormatCSV,
		"application/json": FormatJSON,
		"text/plain":       FormatText,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": FormatDOCX,
	}
	for mt, want := range cases {
		if got, ok := FormatFromMediaType(mt); !ok || got != want {
			t.Errorf("FormatFromMediaType(%q) = %q,%v; want %q", mt, got, ok, want)
		}
	}
	// parameters tolerated; unknown rejected.
	if got, ok := FormatFromMediaType("text/html; charset=utf-8"); !ok || got != FormatHTML {
		t.Errorf("charset param not tolerated: %q,%v", got, ok)
	}
	if _, ok := FormatFromMediaType("application/zip"); ok {
		t.Error("unknown media type must return ok=false")
	}
}
