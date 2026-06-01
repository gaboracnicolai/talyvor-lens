package distill

import (
	"context"
	"strings"
	"testing"
)

// faithful must be byte-for-byte the converter output AND the default. The
// existing golden tests already pin converter output; this pins that the
// default tier doesn't alter it and is recorded as faithful.
func TestTier_FaithfulIsDefaultAndIdentity(t *testing.T) {
	ctx := context.Background()
	in := mustRead(t, "sample.html")

	def, err := DistillAs(ctx, in, FormatHTML) // no option → default
	if err != nil {
		t.Fatal(err)
	}
	if def.Tier != TierFaithful {
		t.Errorf("default tier = %q, want faithful", def.Tier)
	}

	explicit, _ := DistillAs(ctx, in, FormatHTML, WithTier(TierFaithful))
	if explicit.Markdown != def.Markdown {
		t.Errorf("explicit faithful must equal default:\n%q\nvs\n%q", explicit.Markdown, def.Markdown)
	}
	// And faithful equals the raw converter output (no post-processing).
	raw, _ := (htmlConverter{}).Convert(ctx, in)
	if def.Markdown != normalizeText(raw.Markdown) && def.Markdown != raw.Markdown {
		t.Errorf("faithful must equal converter output; got %q", def.Markdown)
	}
}

func TestTier_RecordedInResult(t *testing.T) {
	ctx := context.Background()
	in := mustRead(t, "sample.html")
	for _, tier := range []Tier{TierFaithful, TierStructured, TierOutline} {
		r, err := DistillAs(ctx, in, FormatHTML, WithTier(tier))
		if err != nil {
			t.Fatal(err)
		}
		if r.Tier != tier {
			t.Errorf("Result.Tier = %q, want %q", r.Tier, tier)
		}
	}
	// Unknown/zero tier normalizes to faithful.
	r, _ := DistillAs(ctx, in, FormatHTML, WithTier(Tier("bogus")))
	if r.Tier != TierFaithful {
		t.Errorf("unknown tier should normalize to faithful, got %q", r.Tier)
	}
}

// structured drops decorative content (page markers + repeated running
// headers/footers) while keeping headings, tables, lists, real prose.
func TestStructured_DropsDecoration(t *testing.T) {
	md := strings.Join([]string{
		"# Title",
		"",
		"Page 1",
		"",
		"Running Header Co.",
		"Real content line one.",
		"Running Header Co.",
		"Real content line two.",
		"Running Header Co.",
		"",
		"| a | b |",
		"| --- | --- |",
		"| 1 | 2 |",
	}, "\n")

	got := structuredMarkdown(md)

	for _, dropped := range []string{"Page 1", "Running Header Co."} {
		if strings.Contains(got, dropped) {
			t.Errorf("structured should drop %q:\n%s", dropped, got)
		}
	}
	for _, kept := range []string{"# Title", "Real content line one.", "Real content line two.", "| a | b |", "| 1 | 2 |"} {
		if !strings.Contains(got, kept) {
			t.Errorf("structured should keep %q:\n%s", kept, got)
		}
	}
}

// A frequently-appearing STRUCTURAL line (e.g. a repeated heading) is NOT
// dropped — only non-structural decoration is.
func TestStructured_KeepsStructuralEvenIfRepeated(t *testing.T) {
	md := strings.Join([]string{"## Repeated", "x", "## Repeated", "y", "## Repeated", "z"}, "\n")
	got := structuredMarkdown(md)
	if strings.Count(got, "## Repeated") != 3 {
		t.Errorf("repeated headings must survive structured tier:\n%s", got)
	}
}

// outline keeps the heading hierarchy + a one-line table summary, omits body.
func TestOutline_HeadingsAndTableSummaryOnly(t *testing.T) {
	md := strings.Join([]string{
		"# Doc Title",
		"",
		"Intro paragraph that should be omitted.",
		"",
		"## Section A",
		"",
		"Body text omitted.",
		"",
		"| Name | Age |",
		"| --- | --- |",
		"| Alice | 30 |",
		"| Bob | 25 |",
		"",
		"## Section B",
		"",
		"More body.",
	}, "\n")

	got := outlineMarkdown(md)

	for _, h := range []string{"# Doc Title", "## Section A", "## Section B"} {
		if !strings.Contains(got, h) {
			t.Errorf("outline must keep heading %q:\n%s", h, got)
		}
	}
	for _, omitted := range []string{"Intro paragraph", "Body text omitted", "More body", "Alice", "Bob"} {
		if strings.Contains(got, omitted) {
			t.Errorf("outline must omit body %q:\n%s", omitted, got)
		}
	}
	if !strings.Contains(got, "Table: 2 rows × 2 columns") {
		t.Errorf("outline must summarize the table (2 data rows × 2 cols):\n%s", got)
	}
}

func TestOutline_NoHeadingsNote(t *testing.T) {
	got := outlineMarkdown("just some body text\nwith no headings or tables")
	if !strings.Contains(got, "no headings or tables") {
		t.Errorf("outline of heading-less body should note it; got %q", got)
	}
}

// outline reduces token count vs faithful for a heading-rich document.
func TestOutline_ReducesTokens(t *testing.T) {
	ctx := context.Background()
	in := mustRead(t, "sample.html")
	faithful, _, _ := DistillWithCache(ctx, nil, in, WithTier(TierFaithful))
	outline, _, _ := DistillWithCache(ctx, nil, in, WithTier(TierOutline))
	if len(outline.Markdown) >= len(faithful.Markdown) {
		t.Errorf("outline should be smaller than faithful: outline=%d faithful=%d", len(outline.Markdown), len(faithful.Markdown))
	}
}
