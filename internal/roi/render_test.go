package roi

import (
	"context"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/budgets"
)

func TestRenderHTML_SelfContainedAndComplete(t *testing.T) {
	rep, _ := newReporter(fullMock(), Config{IncludeEngineerBreakdown: true}).GenerateReport(context.Background(), "ws1", "monthly")
	html, err := RenderHTML(rep)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	// Valid, complete document.
	for _, want := range []string{"<!DOCTYPE html>", "</html>", "Executive AI-Cost Report"} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	// Key sections present.
	for _, want := range []string{"Spend by team", "Spend by feature", "Spend by engineer", "Cost per feature", "Budget status", "Forward forecast", "Top cost outliers"} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML missing section %q", want)
		}
	}
	// Self-contained: no external assets.
	for _, bad := range []string{"http://", "https://", "src=", "<link", "<script"} {
		if strings.Contains(html, bad) {
			t.Errorf("HTML must be self-contained; found %q", bad)
		}
	}
	// Statistical-flag framing carried through.
	if !strings.Contains(html, "not verdicts") && !strings.Contains(html, "not a judgment") {
		t.Error("HTML should frame anomalies as flags, not judgments")
	}
}

func TestRenderHTML_EngineerHiddenWhenFlagOff(t *testing.T) {
	rep, _ := newReporter(fullMock(), Config{IncludeEngineerBreakdown: false}).GenerateReport(context.Background(), "ws1", "monthly")
	html, _ := RenderHTML(rep)
	if strings.Contains(html, "Spend by engineer") {
		t.Error("engineer section must be hidden in HTML when the flag is off")
	}
}

func TestRenderMarkdown_ContainsFigures(t *testing.T) {
	rep, _ := newReporter(fullMock(), Config{}).GenerateReport(context.Background(), "ws1", "monthly")
	md := RenderMarkdown(rep)
	for _, want := range []string{
		"# Executive AI-Cost Report",
		"$100.00",       // total
		"Spend by team", // section
		"core",          // a team
		"Statistical flags, not verdicts",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
	// Engineer section omitted when flag off.
	if strings.Contains(md, "Spend by engineer") {
		t.Error("markdown engineer section must be omitted when flag off")
	}
}

func TestRender_ThinDataNotHollow(t *testing.T) {
	m := fullMock()
	m.reconcile = func(b budgets.Budget) float64 { return 0 } // no spend
	rep, _ := newReporter(m, Config{}).GenerateReport(context.Background(), "ws1", "monthly")

	html, err := RenderHTML(rep)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if !strings.Contains(html, rep.DataNote) || !strings.Contains(html, "banner") {
		t.Error("thin-data HTML should show the insufficient-data banner, not hollow tables")
	}
	// No populated breakdown tables in a thin-data report.
	if strings.Contains(html, "Spend by team") {
		t.Error("thin-data HTML must not render breakdown sections")
	}

	md := RenderMarkdown(rep)
	if !strings.Contains(md, rep.DataNote) || strings.Contains(md, "Spend by team") {
		t.Error("thin-data markdown should show the note, not hollow sections")
	}
}
