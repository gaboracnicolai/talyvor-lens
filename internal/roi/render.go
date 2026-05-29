package roi

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
)

// RenderMarkdown renders the report as Markdown — for pasting into
// email/Slack/docs. All figures present; projections/flags keep their
// framing.
func RenderMarkdown(rep ExecReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Executive AI-Cost Report — %s\n\n", rep.WorkspaceID)
	fmt.Fprintf(&b, "**Period:** %s (%s → %s)  \n", rep.Period, rep.PeriodStart.Format("2006-01-02"), rep.PeriodEnd.Format("2006-01-02"))
	fmt.Fprintf(&b, "**Generated:** %s  \n", rep.GeneratedAt.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(&b, "**Total AI spend:** %s\n\n", usd(rep.TotalSpendUSD))

	if rep.InsufficientData {
		fmt.Fprintf(&b, "> ⚠️ %s\n", rep.DataNote)
		return b.String()
	}

	pc := rep.PrevPeriodComparison
	fmt.Fprintf(&b, "**vs previous period:** %s (%+.1f%%) — previous %s\n\n", signedUSD(pc.DeltaUSD), pc.PctChange, usd(pc.PrevTotalUSD))

	b.WriteString("## Spend by team\n\n")
	if len(rep.SpendByTeam) == 0 {
		b.WriteString("_No team-attributed spend._\n\n")
	} else {
		b.WriteString("| Team | Cost | % | Δ vs prev |\n|---|---|---|---|\n")
		for _, t := range rep.SpendByTeam {
			fmt.Fprintf(&b, "| %s | %s | %.1f%% | %s |\n", t.Team, usd(t.CostUSD), t.Pct, signedUSD(t.DeltaVsPrevUSD))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Spend by feature (issue)\n\n")
	if len(rep.SpendByFeature) == 0 {
		b.WriteString("_No issue-attributed spend._\n\n")
	} else {
		b.WriteString("| Issue | Cost | % |\n|---|---|---|\n")
		for _, f := range rep.SpendByFeature {
			fmt.Fprintf(&b, "| %s | %s | %.1f%% |\n", f.IssueID, usd(f.CostUSD), f.Pct)
		}
		b.WriteString("\n")
	}

	if rep.EngineerBreakdownEnabled {
		b.WriteString("## Spend by engineer\n\n")
		b.WriteString("_Cost attribution (which work a dollar landed against), not a performance judgment._\n\n")
		b.WriteString("| Engineer | Cost | Requests |\n|---|---|---|\n")
		for _, e := range rep.SpendByEngineer {
			fmt.Fprintf(&b, "| %s | %s | %d |\n", e.Author, usd(e.CostUSD), e.Requests)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Cost per feature — trend\n\n")
	b.WriteString("| Month | Avg cost / issue | Issues |\n|---|---|---|\n")
	for _, p := range rep.CostPerFeatureTrend {
		fmt.Fprintf(&b, "| %s | %s | %d |\n", p.Period, usd(p.AvgCostUSD), p.FeatureCount)
	}
	b.WriteString("\n")

	b.WriteString("## Budget status\n\n")
	if len(rep.BudgetStatus) == 0 {
		b.WriteString("_No budgets configured._\n\n")
	} else {
		b.WriteString("| Scope | ID | Spent / Limit | Util | Status |\n|---|---|---|---|---|\n")
		for _, s := range rep.BudgetStatus {
			util := "—"
			if s.LimitUSD > 0 {
				util = fmt.Sprintf("%.0f%%", s.Utilization*100)
			}
			fmt.Fprintf(&b, "| %s | %s | %s / %s | %s | %s |\n", s.Scope, s.ScopeID, usd(s.SpentUSD), usd(s.LimitUSD), util, s.Status)
		}
		b.WriteString("\n")
	}

	fs := rep.ForecastSummary
	b.WriteString("## Forward forecast\n\n")
	if fs.InsufficientData {
		fmt.Fprintf(&b, "_Projection unavailable: %s_\n\n", fs.ConfidenceNote)
	} else {
		fmt.Fprintf(&b, "Projected period total: **≈ %s** (a projection, not a guarantee)  \n", usd(fs.ProjectedTotalUSD))
		if fs.LimitUSD > 0 {
			verdict := "within budget"
			if fs.WillExceed {
				verdict = fmt.Sprintf("projected to EXCEED budget by %s", usd(fs.ProjectedOverageUSD))
			}
			fmt.Fprintf(&b, "vs budget %s: %s  \n", usd(fs.LimitUSD), verdict)
		}
		fmt.Fprintf(&b, "_%s_\n\n", fs.ConfidenceNote)
	}

	b.WriteString("## Top cost outliers\n\n")
	if len(rep.Anomalies) == 0 {
		b.WriteString("_None flagged (or baseline too small to judge)._\n\n")
	} else {
		b.WriteString("_Statistical flags, not verdicts._\n\n")
		b.WriteString("| Unit | Cost | × median | Severity |\n|---|---|---|---|\n")
		for _, a := range rep.Anomalies {
			fmt.Fprintf(&b, "| %s | %s | %.1f× | %s |\n", a.UnitID, usd(a.CostUSD), a.Factor, a.Severity)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func usd(v float64) string       { return fmt.Sprintf("$%.2f", v) }
func signedUSD(v float64) string { return fmt.Sprintf("%+.2f", v) } //nolint:gocritic — sign is intentional

// htmlReportTmpl is a self-contained, printable HTML document — inline CSS,
// no external assets, print-to-PDF-able by the recipient.
var htmlReportTmpl = template.Must(template.New("roi").Funcs(template.FuncMap{
	"usd":    func(v float64) string { return fmt.Sprintf("$%.2f", v) },
	"pct":    func(v float64) string { return fmt.Sprintf("%.1f%%", v) },
	"signed": func(v float64) string { return fmt.Sprintf("%+.2f", v) },
	"util": func(limit, u float64) string {
		if limit <= 0 {
			return "—"
		}
		return fmt.Sprintf("%.0f%%", u*100)
	},
}).Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<title>Executive AI-Cost Report — {{.WorkspaceID}}</title>
<style>
  body{font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#1a1a2e;max-width:880px;margin:32px auto;padding:0 24px;line-height:1.5}
  h1{font-size:24px;margin-bottom:4px} h2{font-size:16px;margin-top:28px;border-bottom:1px solid #e0e0ea;padding-bottom:4px}
  .meta{color:#555;font-size:13px} .total{font-size:30px;font-weight:700;margin:8px 0}
  table{border-collapse:collapse;width:100%;font-size:13px;margin-top:8px}
  th,td{text-align:left;padding:6px 10px;border-bottom:1px solid #eee} th{color:#555;font-weight:600}
  td.num{text-align:right;font-variant-numeric:tabular-nums}
  .proj{font-style:italic} .note{color:#666;font-size:12px;font-style:italic}
  .pill{display:inline-block;padding:1px 8px;border-radius:10px;font-size:11px}
  .ok{background:#e6f7ec;color:#137333} .warn{background:#fef7e0;color:#a06000} .over,.high{background:#fce8e6;color:#c5221f}
  .banner{background:#fef7e0;border:1px solid #f0d000;padding:10px 14px;border-radius:6px}
</style></head><body>
<h1>Executive AI-Cost Report</h1>
<div class="meta">{{.WorkspaceID}} · {{.Period}} · {{.PeriodStart.Format "2006-01-02"}} → {{.PeriodEnd.Format "2006-01-02"}} · generated {{.GeneratedAt.Format "2006-01-02 15:04 MST"}}</div>
<div class="total">{{usd .TotalSpendUSD}}</div>
{{if .InsufficientData}}
  <p class="banner">⚠️ {{.DataNote}}</p>
{{else}}
<div class="meta">vs previous period: {{signed .PrevPeriodComparison.DeltaUSD}} ({{printf "%+.1f%%" .PrevPeriodComparison.PctChange}}) — previous {{usd .PrevPeriodComparison.PrevTotalUSD}}</div>

<h2>Spend by team</h2>
<table><tr><th>Team</th><th>Cost</th><th>%</th><th>Δ vs prev</th></tr>
{{range .SpendByTeam}}<tr><td>{{.Team}}</td><td class="num">{{usd .CostUSD}}</td><td class="num">{{pct .Pct}}</td><td class="num">{{signed .DeltaVsPrevUSD}}</td></tr>{{else}}<tr><td colspan="4" class="note">No team-attributed spend.</td></tr>{{end}}
</table>

<h2>Spend by feature (issue)</h2>
<table><tr><th>Issue</th><th>Cost</th><th>%</th></tr>
{{range .SpendByFeature}}<tr><td>{{.IssueID}}</td><td class="num">{{usd .CostUSD}}</td><td class="num">{{pct .Pct}}</td></tr>{{else}}<tr><td colspan="3" class="note">No issue-attributed spend.</td></tr>{{end}}
</table>

{{if .EngineerBreakdownEnabled}}
<h2>Spend by engineer</h2>
<p class="note">Cost attribution (which work a dollar landed against), not a performance judgment.</p>
<table><tr><th>Engineer</th><th>Cost</th><th>Requests</th></tr>
{{range .SpendByEngineer}}<tr><td>{{.Author}}</td><td class="num">{{usd .CostUSD}}</td><td class="num">{{.Requests}}</td></tr>{{end}}
</table>
{{end}}

<h2>Cost per feature — trend</h2>
<table><tr><th>Month</th><th>Avg cost / issue</th><th>Issues</th></tr>
{{range .CostPerFeatureTrend}}<tr><td>{{.Period}}</td><td class="num">{{usd .AvgCostUSD}}</td><td class="num">{{.FeatureCount}}</td></tr>{{end}}
</table>

<h2>Budget status</h2>
<table><tr><th>Scope</th><th>ID</th><th>Spent / Limit</th><th>Util</th><th>Status</th></tr>
{{range .BudgetStatus}}<tr><td>{{.Scope}}</td><td>{{.ScopeID}}</td><td class="num">{{usd .SpentUSD}} / {{usd .LimitUSD}}</td><td class="num">{{util .LimitUSD .Utilization}}</td><td><span class="pill {{.Status}}">{{.Status}}</span></td></tr>{{else}}<tr><td colspan="5" class="note">No budgets configured.</td></tr>{{end}}
</table>

<h2>Forward forecast</h2>
{{with .ForecastSummary}}
  {{if .InsufficientData}}
    <p class="note">Projection unavailable: {{.ConfidenceNote}}</p>
  {{else}}
    <p><span class="proj">≈ {{usd .ProjectedTotalUSD}}</span> projected period total (a projection, not a guarantee).
    {{if gt .LimitUSD 0.0}} vs budget {{usd .LimitUSD}}: {{if .WillExceed}}<span class="pill over">projected to exceed by {{usd .ProjectedOverageUSD}}</span>{{else}}<span class="pill ok">within budget</span>{{end}}{{end}}</p>
    <p class="note">{{.ConfidenceNote}}</p>
  {{end}}
{{end}}

<h2>Top cost outliers</h2>
<p class="note">Statistical flags, not verdicts — a high multiple of the median is a fact, not a judgment that anything is wrong.</p>
<table><tr><th>Unit</th><th>Cost</th><th>× median</th><th>Severity</th></tr>
{{range .Anomalies}}<tr><td>{{.UnitID}}</td><td class="num">{{usd .CostUSD}}</td><td class="num">{{printf "%.1f×" .Factor}}</td><td><span class="pill {{.Severity}}">{{.Severity}}</span></td></tr>{{else}}<tr><td colspan="4" class="note">None flagged (or baseline too small to judge).</td></tr>{{end}}
</table>
{{end}}
<p class="note" style="margin-top:32px">Print to PDF to share. Figures are read-only aggregations of recorded AI spend; projections and statistical flags are labeled as such.</p>
</body></html>`))

// RenderHTML renders the self-contained printable HTML report.
func RenderHTML(rep ExecReport) (string, error) {
	var buf bytes.Buffer
	if err := htmlReportTmpl.Execute(&buf, rep); err != nil {
		return "", err
	}
	return buf.String(), nil
}
