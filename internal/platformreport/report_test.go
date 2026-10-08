package platformreport

import (
	"bytes"
	"encoding/csv"
	"testing"
)

// A seller's name or address that a spreadsheet would run as a formula is written as text.
func TestRender_CSVWritesASellersFormulaAsText(t *testing.T) {
	file, err := Render(Report{Year: 2026, Funding: FundingTest, Currency: "USD", Records: []Record{{WorkspaceID: "ws-1",
		FirstName: `=HYPERLINK("http://x","y")`, Address: "+44 Road", AccountHolder: "Ada", Activity: ActivityDigitalListing,
		Quarters: [4]Quarter{{ConsiderationUSDMicros: -5}}}}}, FormatCSV)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(file)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	r := rows[1]
	if r[col["first_name"]] != `'=HYPERLINK("http://x","y")` || r[col["primary_address"]] != "'+44 Road" ||
		r[col["financial_account_holder"]] != "Ada" || r[col["q1_consideration_usd_micros"]] != "-5" {
		t.Fatalf("cells = name %q, address %q, holder %q, Q1 %q; want the formulas quoted and the figures as numbers",
			r[col["first_name"]], r[col["primary_address"]], r[col["financial_account_holder"]], r[col["q1_consideration_usd_micros"]])
	}
}
