package catalog

import "testing"

// B23.7 — a model its provider retired leaves the chat picker (the suite offers
// `!deprecated && output_per_1m > 0`) and keeps its price, so a request already recorded under it
// still bills the same.
func TestCatalog_RetiredMistralModelsLeaveThePickerAndKeepTheirPrice(t *testing.T) {
	for id, want := range map[string][2]float64{"open-mistral-7b": {0.025, 0.025}, "mistral-nemo": {0.015, 0.045}} {
		m, ok := Get(id)
		if !ok {
			t.Fatalf("%s is gone from the catalog — a past request under it could no longer price", id)
		}
		if !m.Deprecated {
			t.Errorf("%s is not deprecated — the chat picker still offers a model Mistral retired", id)
		}
		if m.InputPer1M != want[0] || m.OutputPer1M != want[1] {
			t.Errorf("%s prices %v/%v, want %v/%v — no price changes here", id, m.InputPer1M, m.OutputPer1M, want[0], want[1])
		}
	}
}
